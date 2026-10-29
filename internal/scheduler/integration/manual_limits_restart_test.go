package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestManualLimitControlPlaneProcess(t *testing.T) {

	path := os.Getenv("MERGEYARD_TEST_MANUAL_LIMIT_PROCESS")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var input testkit.ManualLimitProcessInput
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	rt, err := app.Open(context.Background(), input.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}}
	api.Head = func(branch string) string {
		return testkit.GitCommand(t, input.Remote, "rev-parse", "refs/heads/"+branch)
	}
	var r runner.Runner = testkit.NewLocalRunner(t, runner.Options{SocketName: input.Socket})
	if input.Boundary == "takeover" {
		r = testkit.TakeoverCrashRunner{Runner: r}
	}
	s, err := testkit.TestScheduler(input.Config, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r, Git: testkit.ManualLimitCrashGit{Manager: testkit.TestGitManager(rt.Workspace), Boundary: input.Boundary}, Now: func() time.Time { return input.Now }})
	if err != nil {
		t.Fatal(err)
	}
	if input.Boundary == "takeover" {
		if _, err := s.Takeover(context.Background(), input.RunID); err != nil {
			t.Fatal(err)
		}
		t.Fatal("did not stop at takeover boundary")
	}
	if _, err := s.Handback(context.Background(), input.RunID); err != nil {
		t.Fatal(err)
	}
	if input.Boundary != "selected" {
		t.Fatal("did not stop at publication boundary")
	}
	fmt.Println("manual-limit-control-plane-ready")
	select {}
}

func TestBlockedHandbackRecoversAfterControlPlaneKill(t *testing.T) {

	for _, agent := range []string{"claude", "codex"} {
		for _, kind := range []harness.FailureKind{harness.TemporaryLimit, harness.CreditsExhausted} {
			for _, boundary := range []string{"commit", "push", "selected"} {
				t.Run(agent+"/"+string(kind)+"/"+boundary, func(t *testing.T) {

					_, rt, api, remote, cfg, _ := testkit.TmuxPairingFlow(t, agent, agent, testkit.DisputedReport)
					testkit.ReplacePhaseScript(t, cfg, workflow.Fix, `printf partial > partial.txt; exit 1`)
					socket := fmt.Sprintf("mergeyard-manual-limit-%d", time.Now().UnixNano())
					t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
					r := testkit.NewLocalRunner(t, runner.Options{SocketName: socket})
					now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
					h := testkit.ManualLimitHarness(cfg, agent, kind)
					deps := scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{agent: h}}
					s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
					if err != nil {
						t.Fatal(err)
					}
					state := workflow.WaitingForHarness
					if kind == harness.CreditsExhausted {
						state = workflow.NeedsAttention
					}
					before := testkit.Finish(t, s, state, workflow.Implement)
					prepared, err := s.Takeover(context.Background(), before.ID)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(testkit.RunWorktree(t, rt.Workspace.Root, prepared), "manual.txt"), []byte("manual work"), 0600); err != nil {
						t.Fatal(err)
					}
					root := rt.Workspace.Root
					if err := rt.Close(); err != nil {
						t.Fatal(err)
					}
					testkit.KillManualLimitControlPlane(t, testkit.ManualLimitProcessInput{Config: cfg, Workspace: root, Socket: socket, Remote: remote, RunID: before.ID, Boundary: boundary, Now: now})
					rt, err = app.Open(context.Background(), root)
					if err != nil {
						t.Fatal(err)
					}
					defer rt.Close()
					s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
					if err != nil {
						t.Fatal(err)
					}
					for range 2 {
						if _, err := s.Reconcile(context.Background()); err != nil {
							t.Fatal(err)
						}
					}
					selected, err := s.Handback(context.Background(), before.ID)
					if err != nil || selected.State != workflow.WaitingForHarness || selected.Phase != workflow.Implement || len(selected.Handbacks) != 1 || selected.Handbacks[0].Pending || selected.Handbacks[0].NextState != workflow.WaitingForHarness || selected.Implementer.SessionID != before.Implementer.SessionID || len(h.Phases) != 1 {
						t.Fatalf("restart lost selection or duplicated launch: %+v %v", selected, err)
					}
					if testkit.GitCommand(t, remote, "show", "mergeyard/issue-7:manual.txt") != "manual work" || testkit.GitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7") != "1" {
						t.Fatal("duplicated/overwritten manual work")
					}
					history, err := rt.Events.History(context.Background(), 0, 100)
					if err != nil {
						t.Fatal(err)
					}
					handed := 0
					for _, event := range history {
						if event.Type == "run.handed_back" {
							handed++
						}
					}
					if handed != 1 {
						t.Fatalf("duplicate handback: %d", handed)
					}
				})
			}
		}
	}
}

func TestCreditProbeTakeoverRecoversAfterControlPlaneKill(t *testing.T) {

	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {

			_, rt, api, remote, cfg, _ := testkit.TmuxPairingFlow(t, agent, agent, testkit.DisputedReport)
			testkit.ReplacePhaseScript(t, cfg, workflow.Fix, `exit 1`)
			socket := fmt.Sprintf("mergeyard-probe-takeover-%d", time.Now().UnixNano())
			t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
			r := testkit.NewLocalRunner(t, runner.Options{SocketName: socket})
			h := testkit.ManualLimitHarness(cfg, agent, harness.CreditsExhausted)
			deps := scheduler.Dependencies{GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{agent: h}}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
			if err != nil {
				t.Fatal(err)
			}
			before := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
			if _, err := s.Retry(context.Background(), before.ID); err != nil {
				t.Fatal(err)
			}
			testkit.ReplacePhaseScript(t, cfg, workflow.Fix, `printf probe > probe.txt; sleep 60`)
			path := filepath.Join(rt.Workspace.Root, "worktrees", "owner-repo", before.ID)
			testkit.WaitForWithin(t, 30*time.Second, func() bool {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				_, err := os.Stat(filepath.Join(path, "probe.txt"))
				return err == nil
			})
			root := rt.Workspace.Root
			if err := rt.Close(); err != nil {
				t.Fatal(err)
			}
			testkit.KillManualLimitControlPlane(t, testkit.ManualLimitProcessInput{Config: cfg, Workspace: root, Socket: socket, Remote: remote, RunID: before.ID, Boundary: "takeover"})
			rt, err = app.Open(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			defer rt.Close()
			pending, err := rt.Workflow.Get(context.Background(), before.ID)
			if err != nil || pending.TakeoverStatus != workflow.TakeoverRequested || pending.CreditProbes[0].Status != "running" {
				t.Fatalf("lost takeover intent: %+v %v", pending, err)
			}
			s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			manual, err := rt.Workflow.Get(context.Background(), before.ID)
			if err != nil || manual.State != workflow.Manual || manual.Implementer.Attempt != 2 || manual.CreditProbes[0].Status != "released" || len(h.Phases) != 2 {
				t.Fatalf("restart duplicated probe: %+v %v", manual, err)
			}
			states, err := s.HarnessAvailability(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, state := range states {
				if state.Harness == agent && (state.Available || state.ProbeID != "") {
					t.Fatalf("restart cleared block: %+v", state)
				}
			}
			if data, err := os.ReadFile(filepath.Join(path, "probe.txt")); err != nil || string(data) != "probe" {
				t.Fatalf("lost probe work: %q %v", data, err)
			}
		})
	}
}
