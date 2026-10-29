package integration_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// Exercise restrictions through normalized fake adapters, never guessed native
// limit signals. All repositories, worktrees, SQLite and tmux executions are real.
func TestCombinedRecoveryAcrossRepositories(t *testing.T) {

	for _, blocked := range []string{"claude", "codex"} {
		for _, kind := range []harness.FailureKind{harness.TemporaryLimit, harness.CreditsExhausted} {
			t.Run(blocked+"/"+string(kind), func(t *testing.T) {

				ctx := context.Background()
				other := "codex"
				if blocked == other {
					other = "claude"
				}
				_, rt, base, remote, cfg, _ := testkit.TmuxPairingFlow(t, blocked, other, testkit.DisputedReport)
				socket := fmt.Sprintf("mergeyard-combined-%d", time.Now().UnixNano())
				t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
				r := testkit.NewLocalRunner(t, runner.Options{SocketName: socket})
				cfg.Concurrency = 3
				cfg.Repositories[0].Concurrency = 1
				remotes := map[string]string{"mergeyard/issue-7": remote}
				for i, name := range []string{"mixed", "other", "overflow"} {
					repo := cfg.Repositories[0]
					repo.Repo = "owner/" + name
					repo.Implementer.Agent = other
					repo.Reviewer.Agent = other
					if name == "mixed" {
						repo.Reviewer.Agent = blocked
					}
					cfg.Repositories = append(cfg.Repositories, repo)
					path := filepath.Join(t.TempDir(), "remote.git")
					testkit.GitCommand(t, filepath.Dir(path), "clone", "--bare", remote, path)
					testkit.GitCommand(t, filepath.Dir(remote), "config", "--file", testkit.TestGitConfig(t), "--add", "url."+path+".insteadOf", "https://github.com/"+repo.Repo+".git")
					remotes[fmt.Sprintf("mergeyard/issue-%d", 8+i)] = path
				}
				remotes["mergeyard/issue-11"] = remotes["mergeyard/issue-9"]
				remotes["mergeyard/issue-12"] = remotes["mergeyard/issue-9"]
				base.Head = func(branch string) string {
					return testkit.GitCommand(t, remotes[branch], "rev-parse", "refs/heads/"+branch)
				}
				// First implementation on the blocked harness fails with preserved edits.
				testkit.ReplacePhaseScript(t, cfg, workflow.Implement, `case "$*" in *implement-0-1*) printf partial > partial.txt; exit 1;; esac
`+testkit.ReviewScript(testkit.ApprovedReview))
				// The other harness is independently usable. A later marker holds new
				// implementations so capacity can be inspected without timing races.
				gate := filepath.Join(t.TempDir(), "hold")
				otherScript := `case "$*" in *implement-*) while [ -f '` + gate + `' ]; do /bin/sleep 0.02; done;; esac
` + testkit.ReviewScript(testkit.ApprovedReview)
				executable := cfg.Agents.Claude.Executable
				if other == "codex" {
					executable = cfg.Agents.Codex.Executable
					otherScript = testkit.NativeCodexScript(otherScript)
				}
				if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+otherScript), 0700); err != nil {
					t.Fatal(err)
				}
				calls := filepath.Join(t.TempDir(), "launches")
				for _, executable := range []string{cfg.Agents.Claude.Executable, cfg.Agents.Codex.Executable} {
					data, err := os.ReadFile(executable)
					if err != nil {
						t.Fatal(err)
					}
					data = []byte(strings.Replace(string(data), "#!/bin/sh\n", "#!/bin/sh\nprintf '%s\\n' \"$PWD $*\" >> '"+calls+"'\n", 1))
					if err := os.WriteFile(executable, data, 0700); err != nil {
						t.Fatal(err)
					}
				}
				now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
				h := testkit.ManualLimitHarness(cfg, blocked, kind)
				api := testkit.CombinedGitHub{CiGitHub: &testkit.CiGitHub{FakeGitHub: base, ApplyReady: true, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "success"}}}}}
				deps := scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{blocked: h}}
				s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
				if err != nil {
					t.Fatal(err)
				}
				state := workflow.WaitingForHarness
				if kind == harness.CreditsExhausted {
					state = workflow.NeedsAttention
				}
				origin := testkit.Finish(t, s, state, workflow.Implement)
				base.Issues["owner/repo"] = append(base.Issues["owner/repo"], testkit.Ready(13))
				base.Issues["owner/mixed"] = []github.Issue{testkit.Ready(8)}
				base.Issues["owner/other"] = []github.Issue{testkit.Ready(9)}
				var mixed, independent workflow.Run
				testkit.WaitForWithin(t, 30*time.Second, func() bool {
					if err := s.Tick(ctx); err != nil {
						t.Fatal(err)
					}
					runs := testkit.CombinedRuns(t, s)
					mixed, independent = runs[8], runs[9]
					return mixed.State == workflow.WaitingForHarness && independent.State == workflow.ReadyToMerge
				})
				if mixed.Phase != workflow.Review || mixed.HarnessWait.Harness != blocked || mixed.HarnessWait.AttemptID != "" || mixed.Review != nil || independent.ApprovedSHA != base.Head("mergeyard/issue-9") || base.Prs["mergeyard/issue-9"].Merged {
					t.Fatalf("mixed-role gate or independent readiness: mixed=%+v independent=%+v", mixed, independent)
				}
				if err := os.WriteFile(gate, nil, 0600); err != nil {
					t.Fatal(err)
				}
				// READY_TO_MERGE releases both slots; the next run in that same
				// repository claims the slot, while its second issue and overflow wait.
				base.Issues["owner/other"] = append(base.Issues["owner/other"], testkit.Ready(11), testkit.Ready(12))
				base.Issues["owner/overflow"] = []github.Issue{testkit.Ready(10)}
				for range 2 {
					if err := s.Tick(ctx); err != nil {
						t.Fatal(err)
					}
				}
				runs := testkit.CombinedRuns(t, s)
				if len(runs) != 4 || runs[11].State != workflow.Active || runs[12].ID != "" || runs[10].ID != "" || runs[13].ID != "" {
					t.Fatalf("global/repository caps or blocked implementer bypassed: %+v", runs)
				}
				if _, err := s.Takeover(ctx, origin.ID); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(testkit.RunWorktree(t, rt.Workspace.Root, origin), "manual.txt"), []byte("manual work"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := s.Tick(ctx); err != nil {
					t.Fatal(err)
				}
				runs = testkit.CombinedRuns(t, s)
				if runs[7].State != workflow.Manual || runs[10].ID != "" || len(h.Phases) != 1 {
					t.Fatalf("manual slot lost or automated phase launched: %+v", runs)
				}
				// A real killed control plane observes manual, account-waiting and
				// live independent work together. Its restart must adopt the live
				// execution and leave manual work untouched.
				restart := func(boundary string) {
					t.Helper()
					root := rt.Workspace.Root
					if err := rt.Close(); err != nil {
						t.Fatal(err)
					}
					testkit.KillCombinedControlPlane(t, testkit.CombinedProcessInput{Config: cfg, Workspace: root, Socket: socket, Blocked: blocked, Kind: kind, Now: now, RunID: origin.ID, Boundary: boundary, Issues: base.Issues, PRs: base.Prs, Remotes: remotes})
					rt, err = app.Open(ctx, root)
					if err != nil {
						t.Fatal(err)
					}
					reopened := rt
					t.Cleanup(func() { reopened.Close() })
					s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
					if err != nil {
						t.Fatal(err)
					}
				}
				ref, err := s.Watch(ctx, runs[11].ID)
				if err != nil {
					t.Fatal(err)
				}
				restart("observe")
				if _, err := s.Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
				again, err := s.Watch(ctx, runs[11].ID)
				if err != nil || again != ref || testkit.CombinedRuns(t, s)[7].State != workflow.Manual {
					t.Fatalf("restart duplicated live work or resumed manual: %+v %v", again, err)
				}
				if err := s.Stop(ctx, runs[11].ID); err != nil {
					t.Fatal(err)
				}
				// Disable the full repository's queue to isolate the global slot.
				base.Issues["owner/other"] = base.Issues["owner/other"][:2]
				if err := s.Tick(ctx); err != nil {
					t.Fatal(err)
				}
				runs = testkit.CombinedRuns(t, s)
				if runs[10].State != workflow.Active || runs[11].State != workflow.Stopped {
					t.Fatalf("Stop did not release global capacity: %+v", runs)
				}
				if err := s.Stop(ctx, runs[10].ID); err != nil {
					t.Fatal(err)
				}
				restart("handback")
				selected, err := s.Handback(ctx, origin.ID)
				if err != nil || selected.State != workflow.WaitingForHarness || selected.Phase != workflow.Implement {
					t.Fatalf("blocked handback: %+v %v", selected, err)
				}
				if kind == harness.TemporaryLimit {
					now = selected.HarnessWait.ResetAt
				} else {
					if _, err := s.Retry(ctx, origin.ID); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Remove(gate); err != nil {
					t.Fatal(err)
				}
				// Prevent unrelated queue work from obscuring recovery assertions.
				base.Issues["owner/repo"] = base.Issues["owner/repo"][:1]
				restart("resume")
				testkit.WaitForWithin(t, 30*time.Second, func() bool {
					if err := s.Tick(ctx); err != nil {
						t.Fatal(err)
					}
					runs = testkit.CombinedRuns(t, s)
					return runs[7].State == workflow.ReadyToMerge && runs[8].State == workflow.ReadyToMerge
				})
				if runs[7].Implementer.Attempt != 2 || runs[7].Implementer.SessionID != origin.Implementer.SessionID || runs[7].Review.SessionID == origin.Implementer.SessionID || runs[8].Review.Attempt != 1 || len(runs[7].Handbacks) != 1 || base.Creations != 3 {
					t.Fatalf("duplicated recovery or lost role ownership: %+v", runs)
				}
				if kind == harness.CreditsExhausted && (len(runs[7].CreditProbes) != 1 || runs[7].CreditProbes[0].Status != "recovered") {
					t.Fatalf("probe proof lost: %+v", runs[7].CreditProbes)
				}
				if testkit.GitCommand(t, remote, "show", "mergeyard/issue-7:manual.txt") != "manual work" || testkit.GitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7") != "2" {
					t.Fatal("manual work overwritten or duplicate publication")
				}
				data, err := os.ReadFile(calls)
				if err != nil || len(strings.Split(strings.TrimSpace(string(data)), "\n")) != 9 {
					t.Fatalf("duplicated phase launch across crashes: %s %v", data, err)
				}
				for _, n := range []int{10, 11} {
					if _, err := os.Stat(filepath.Join(rt.Workspace.Root, "worktrees", strings.ReplaceAll(runs[n].Repository, "/", "-"), runs[n].ID)); err != nil {
						t.Fatalf("stopped worktree deleted: %v", err)
					}
				}
			})
		}
	}
}
