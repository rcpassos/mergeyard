package integration_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestClearingConfiguredListsWaitsForLiveOwnedStepsAfterRestart(t *testing.T) {

	for _, mode := range []string{"implement", "fix", "handback", "setup"} {
		for _, outcome := range []string{"passed", "failed", "unknown"} {
			t.Run(mode+"/"+outcome, func(t *testing.T) {

				release := filepath.Join(t.TempDir(), "release")
				script := testkit.SuccessfulScript
				if mode == "fix" {
					script = testkit.LoopScript("printf fixed > feature.txt; " + testkit.FixedReport)
				}
				if mode == "setup" {
					script = "[ -f setup-ready ] || exit 9\n" + testkit.SuccessfulScript
				}
				_, rt, api, remote, cfg, r := testkit.TmuxFlow(t, script)
				cfg.Repositories[0].CommandTimeout = 2 * time.Minute
				cfg.Repositories[0].CheckRetries = 0
				waitCommand := "while [ ! -f '" + release + "' ]; do /bin/sleep 0.02; done; printf old-step-finished"
				if outcome == "failed" {
					waitCommand += "; exit 7"
				}
				switch mode {
				case "setup":
					cfg.Repositories[0].Setup = []string{waitCommand + "; printf ready > setup-ready"}
				case "fix":
					cfg.Repositories[0].Checks = []string{"if /usr/bin/grep -q fixed feature.txt; then " + waitCommand + "; fi"}
				case "handback":
					cfg.Repositories[0].Checks = []string{"if /usr/bin/grep -q manual feature.txt; then " + waitCommand + "; fi"}
				default:
					cfg.Repositories[0].Checks = []string{waitCommand}
				}
				deps := scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r}
				s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "handback" {
					before := testkit.Finish(t, s, workflow.Active, workflow.Review)
					if _, err := s.Takeover(context.Background(), before.ID); err != nil {
						t.Fatal(err)
					}
					tree := testkit.SetupWorktree(t, rt, before.ID)
					if err := r.WriteFile(context.Background(), tree+"/feature.txt", []byte("manual"), 0600); err != nil {
						t.Fatal(err)
					}
					if _, err := s.Handback(context.Background(), before.ID); err != nil {
						t.Fatal(err)
					}
				}
				var before workflow.Run
				var owner workflow.SystemStepSnapshot
				testkit.WaitForWithin(t, 15*time.Second, func() bool {
					if err := s.Tick(context.Background()); err != nil {
						t.Fatal(err)
					}
					runs, err := s.Runs(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					before = runs[0]
					for _, step := range before.SystemSteps {
						if step.Status != "running" {
							continue
						}
						if mode == "setup" && step.Step == "setup" {
							owner = step
							return true
						}
						if step.Step != "checks" {
							continue
						}
						if mode == "fix" && step.Phase != workflow.Fix {
							continue
						}
						if mode == "handback" && !strings.HasPrefix(step.OwnerID, "handback:") {
							continue
						}
						owner = step
						return true
					}
					return false
				})
				ref, err := s.Watch(context.Background(), before.ID)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := r.StopSession(context.Background(), ref); err != nil {
						t.Errorf("owned step cleanup: %v", err)
					}
					status, err := r.SessionStatus(context.Background(), ref)
					if err != nil || status.State == "running" {
						t.Errorf("owned step group survived cleanup: %+v %v", status, err)
					}
				})
				if outcome == "unknown" {
					if err := r.StopSession(context.Background(), ref); err != nil {
						t.Fatal(err)
					}
					if err := r.RemovePath(context.Background(), filepath.Join(ref.PhaseDir, "exit.json")); err != nil {
						t.Fatal(err)
					}
				}
				root := rt.Workspace.Root
				if err := rt.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := app.Open(context.Background(), root)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { reopened.Close() })
				cfg.Repositories[0].Checks = nil
				cfg.Repositories[0].Setup = nil
				cfg.Repositories[0].CommandTimeout = time.Millisecond
				restarted, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(reopened), deps)
				if err != nil {
					t.Fatal(err)
				}
				if err := restarted.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				runs, err := restarted.Runs(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				waiting := runs[0]
				live, err := restarted.Watch(context.Background(), before.ID)
				wrongSession := live.Name != ref.Name
				if outcome == "unknown" {
					wrongSession = live.Name == ref.Name || live.Name == ""
				}
				if err != nil || wrongSession || waiting.Phase != before.Phase || (mode == "fix" && waiting.Fix.Pushed) || (mode == "handback" && waiting.PendingHandback() == nil) || (mode == "setup" && waiting.Implementer != nil) {
					t.Fatalf("cleared list bypassed live owned step: mode=%s run=%+v watch=%+v err=%v", mode, waiting, live, err)
				}
				if mode == "implement" && (api.Creations != 0 || testkit.GitCommand(t, remote, "branch", "--list", "mergeyard/issue-7") != "") {
					t.Fatal("implementation published while removed check remained live")
				}
				if err := r.WriteFile(context.Background(), release, nil, 0600); err != nil {
					t.Fatal(err)
				}
				phase := workflow.Review
				state := workflow.Active
				if mode == "fix" {
					state = workflow.WaitingForCI
				}
				if outcome == "failed" {
					state = workflow.NeedsAttention
					phase = before.Phase
				}
				completed := testkit.Finish(t, restarted, state, phase)
				if outcome == "failed" {
					code := "checks.failed"
					if mode == "setup" {
						code = "workspace.setup_failed"
					}
					if completed.LastErrorCode != code || (mode == "setup" && completed.Implementer != nil) || (mode == "fix" && completed.Fix.Pushed) || (mode == "handback" && len(completed.FixHistory) > 0) {
						t.Fatalf("removed list hid failure or spent a retry: %+v", completed)
					}
				}
				found := false
				for _, step := range completed.SystemSteps {
					if step.ID == owner.ID {
						found = true
						want := "succeeded"
						if outcome != "passed" {
							want = "failed"
						}
						if step.Status != want || step.CommandTimeout != "2m0s" {
							t.Fatalf("pending execution result lost: %+v", step)
						}
					}
				}
				wantCount := len(before.SystemSteps)
				if outcome == "unknown" {
					wantCount++
					replay := completed.SystemSteps[len(completed.SystemSteps)-1]
					if replay.Status != "succeeded" || replay.CommandTimeout != "2m0s" || len(replay.Commands) != 1 || replay.Commands[0] != owner.Commands[0] {
						t.Fatalf("unknown outcome did not replay original configured execution: %+v", replay)
					}
				}
				if !found || len(completed.SystemSteps) != wantCount {
					t.Fatalf("cleared list created or lost executions: %+v", completed.SystemSteps)
				}
			})
		}
	}
}

func TestClearedChecksStillSupplyRecordedResultsToReview(t *testing.T) {
	t.Parallel()

	reviewer := `phase_dir=''; previous=''; for arg do if [ "$previous" = '--add-dir' ]; then phase_dir=$arg; fi; previous=$arg; done
if ! /usr/bin/grep -q 'old-step-finished' "$phase_dir/input.md"; then
 printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"schema_version":4,"criteria":[],"human_action":null,"prior_findings":[],"status":"failed","summary":"Missing completed check evidence","findings":[]}}'
else
` + testkit.ApprovedReview + `
fi`
	_, rt, api, _, cfg, r := testkit.TmuxFlow(t, testkit.ReviewScript(reviewer))
	release := filepath.Join(t.TempDir(), "release")
	cfg.Repositories[0].Checks = []string{"printf old-step-finished; while [ ! -f '" + release + "' ]; do /bin/sleep 0.02; done"}
	deps := scheduler.Dependencies{GitHub: api, Runner: r}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WaitForWithin(t, 15*time.Second, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return len(runs) == 1 && len(runs[0].SystemSteps) == 1 && runs[0].SystemSteps[0].Status == "running"
	})
	root := rt.Workspace.Root
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := app.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	cfg.Repositories[0].Checks = nil
	s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(reopened), deps)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.WriteFile(context.Background(), release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	var run workflow.Run
	testkit.WaitForWithin(t, 15*time.Second, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		run = runs[0]
		return run.State == workflow.WaitingForCI || run.State == workflow.NeedsAttention
	})
	if run.State != workflow.WaitingForCI {
		t.Fatalf("removed list hid recorded results from reviewer: %+v", run)
	}
}
