package scheduler_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/config"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type manualLimitProcessInput struct {
	Config                                     config.Config
	Workspace, Socket, Remote, RunID, Boundary string
	Now                                        time.Time
}

type manualLimitCrashGit struct {
	*managedgit.Manager
	boundary string
}

func (g manualLimitCrashGit) CommitHandback(ctx context.Context, run managedgit.Run, previous string) (managedgit.CommitResult, error) {
	result, err := g.Manager.CommitHandback(ctx, run, previous)
	if err == nil && g.boundary == "commit" {
		fmt.Println("manual-limit-control-plane-ready")
		select {}
	}
	return result, err
}

func (g manualLimitCrashGit) PushHandback(ctx context.Context, run managedgit.Run, previous, target string) error {
	err := g.Manager.PushHandback(ctx, run, previous, target)
	if err == nil && g.boundary == "push" {
		fmt.Println("manual-limit-control-plane-ready")
		select {}
	}
	return err
}

func TestManualLimitControlPlaneProcess(t *testing.T) {
	path := os.Getenv("MERGEYARD_TEST_MANUAL_LIMIT_PROCESS")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var input manualLimitProcessInput
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	rt, err := app.Open(context.Background(), input.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}}
	api.head = func(branch string) string { return gitCommand(t, input.Remote, "rev-parse", "refs/heads/"+branch) }
	var r runner.Runner = runner.NewLocal(runner.Options{SocketName: input.Socket})
	if input.Boundary == "takeover" {
		r = takeoverCrashRunner{r}
	}
	s, err := scheduler.New(input.Config, schedulerResources(rt), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r, Git: manualLimitCrashGit{managedgit.New(rt.Workspace), input.Boundary}, Now: func() time.Time { return input.Now }})
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

func killManualLimitControlPlane(t *testing.T, input manualLimitProcessInput) {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "manual-limit-process.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestManualLimitControlPlaneProcess$")
	cmd.Env = append(os.Environ(), "MERGEYARD_TEST_MANUAL_LIMIT_PROCESS="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	expected := "manual-limit-control-plane-ready"
	if input.Boundary == "takeover" {
		expected = "review-control-plane-ready"
	}
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != expected {
		t.Fatalf("crash boundary not reached: %q %v", scanner.Text(), scanner.Err())
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
}

func TestBlockedHandbackRecoversAfterControlPlaneKill(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		for _, kind := range []harness.FailureKind{harness.TemporaryLimit, harness.CreditsExhausted} {
			for _, boundary := range []string{"commit", "push", "selected"} {
				t.Run(agent+"/"+string(kind)+"/"+boundary, func(t *testing.T) {
					_, rt, api, remote, cfg, _ := pairingFlow(t, agent, agent, disputedReport)
					replacePhaseScript(t, cfg, workflow.Fix, `printf partial > partial.txt; exit 1`)
					socket := fmt.Sprintf("mergeyard-manual-limit-%d", time.Now().UnixNano())
					t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
					r := runner.NewLocal(runner.Options{SocketName: socket})
					now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
					h := manualLimitHarness(cfg, agent, kind)
					deps := scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{agent: h}}
					s, err := scheduler.New(cfg, schedulerResources(rt), deps)
					if err != nil {
						t.Fatal(err)
					}
					state := workflow.WaitingForHarness
					if kind == harness.CreditsExhausted {
						state = workflow.NeedsAttention
					}
					before := finish(t, s, state, workflow.Implement)
					command, err := s.Takeover(context.Background(), before.ID)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(command.Dir, "manual.txt"), []byte("manual work"), 0600); err != nil {
						t.Fatal(err)
					}
					root := rt.Workspace.Root
					if err := rt.Close(); err != nil {
						t.Fatal(err)
					}
					killManualLimitControlPlane(t, manualLimitProcessInput{Config: cfg, Workspace: root, Socket: socket, Remote: remote, RunID: before.ID, Boundary: boundary, Now: now})
					rt, err = app.Open(context.Background(), root)
					if err != nil {
						t.Fatal(err)
					}
					defer rt.Close()
					s, err = scheduler.New(cfg, schedulerResources(rt), deps)
					if err != nil {
						t.Fatal(err)
					}
					for range 2 {
						if _, err := s.Reconcile(context.Background()); err != nil {
							t.Fatal(err)
						}
					}
					selected, err := s.Handback(context.Background(), before.ID)
					if err != nil || selected.State != workflow.WaitingForHarness || selected.Phase != workflow.Implement || len(selected.Handbacks) != 1 || selected.Handbacks[0].Pending || selected.Handbacks[0].NextState != workflow.WaitingForHarness || selected.Implementer.SessionID != before.Implementer.SessionID || len(h.phases) != 1 {
						t.Fatalf("restart lost selection or duplicated launch: %+v %v", selected, err)
					}
					if gitCommand(t, remote, "show", "mergeyard/issue-7:manual.txt") != "manual work" || gitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7") != "1" {
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
			_, rt, api, remote, cfg, _ := pairingFlow(t, agent, agent, disputedReport)
			replacePhaseScript(t, cfg, workflow.Fix, `exit 1`)
			socket := fmt.Sprintf("mergeyard-probe-takeover-%d", time.Now().UnixNano())
			t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
			r := runner.NewLocal(runner.Options{SocketName: socket})
			h := manualLimitHarness(cfg, agent, harness.CreditsExhausted)
			deps := scheduler.Dependencies{GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{agent: h}}
			s, err := scheduler.New(cfg, schedulerResources(rt), deps)
			if err != nil {
				t.Fatal(err)
			}
			before := finish(t, s, workflow.NeedsAttention, workflow.Implement)
			if _, err := s.Retry(context.Background(), before.ID); err != nil {
				t.Fatal(err)
			}
			replacePhaseScript(t, cfg, workflow.Fix, `printf probe > probe.txt; sleep 60`)
			path := filepath.Join(rt.Workspace.Root, "worktrees", "owner-repo", before.ID)
			waitForWithin(t, 30*time.Second, func() bool {
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
			killManualLimitControlPlane(t, manualLimitProcessInput{Config: cfg, Workspace: root, Socket: socket, Remote: remote, RunID: before.ID, Boundary: "takeover"})
			rt, err = app.Open(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			defer rt.Close()
			pending, err := rt.Workflow.Get(context.Background(), before.ID)
			if err != nil || pending.TakeoverStatus != workflow.TakeoverRequested || pending.CreditProbes[0].Status != "running" {
				t.Fatalf("lost takeover intent: %+v %v", pending, err)
			}
			s, err = scheduler.New(cfg, schedulerResources(rt), deps)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			manual, err := rt.Workflow.Get(context.Background(), before.ID)
			if err != nil || manual.State != workflow.Manual || manual.Implementer.Attempt != 2 || manual.CreditProbes[0].Status != "released" || len(h.phases) != 2 {
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
