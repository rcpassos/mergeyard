package integration_test

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
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestReviewControlPlaneProcess(t *testing.T) {

	path := os.Getenv("MERGEYARD_TEST_REVIEW_PROCESS")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var input testkit.ReviewProcessInput
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	runtime, err := app.Open(context.Background(), input.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}, Prs: map[string]*github.PullRequest{"mergeyard/issue-7": input.PR}}
	var gh scheduler.GitHub = api
	if input.Remote != "" {
		api.Head = func(branch string) string {
			return testkit.GitCommand(t, input.Remote, "rev-parse", "refs/heads/"+branch)
		}
		gh = testkit.BranchGitHub{FakeGitHub: api}
	}
	var g scheduler.Git = testkit.TestGitManager(runtime.Workspace)
	if input.KillDuringRestore {
		g = testkit.InterruptedRestoreGit{Manager: testkit.TestGitManager(runtime.Workspace)}
	}
	if input.KillDuringFix == "commit" || input.KillDuringFix == "push" {
		g = testkit.InterruptedFixGit{Manager: testkit.TestGitManager(runtime.Workspace), Stage: input.KillDuringFix}
	}
	var phaseRunner runner.Runner = testkit.NewLocalRunner(t, runner.Options{SocketName: input.Socket})
	if input.TakeoverCrash == "stopped" {
		phaseRunner = testkit.TakeoverCrashRunner{Runner: phaseRunner}
	}
	if input.RecoveryCrash != "" {
		phaseRunner = testkit.RecoveryCrashRunner{Runner: phaseRunner, Phase: input.RecoveryPhase, Mode: input.RecoveryCrash}
	}
	s, err := testkit.TestScheduler(input.Config, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: gh, Git: g, Runner: phaseRunner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if input.TakeoverCrash != "" {
		runs, err := s.Runs(context.Background())
		if err != nil || len(runs) != 1 {
			t.Fatalf("takeover fixture runs: %+v %v", runs, err)
		}
		if _, err := s.Takeover(context.Background(), runs[0].ID); err != nil {
			t.Fatal(err)
		}
		t.Fatal("takeover crash boundary was not reached")
	}
	if !input.KillDuringRestore && input.KillDuringFix == "" && input.RecoveryCrash == "" {
		fmt.Println("review-control-plane-ready")
		time.Sleep(time.Minute)
		return
	}
	for {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		if input.RecoveryCrash == "after-identity" {
			runs, err := s.Runs(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(runs) == 1 && len(runs[0].SessionRecoveries) == 1 && runs[0].SessionRecoveries[0].SessionID != "" {
				fmt.Println("review-control-plane-ready")
				select {}
			}
		}
		if input.KillDuringFix == "next review" {
			runs, err := s.Runs(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(runs) == 1 && runs[0].ReviewRound == 2 && runs[0].Phase == workflow.Review {
				fmt.Println("review-control-plane-ready")
				select {}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReviewRecoversAfterControlPlaneKill(t *testing.T) {
	t.Parallel()

	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			for _, mode := range []string{"still running", "finished offline", "restore pending"} {
				t.Run(implementer+"/"+reviewer+"/"+mode, func(t *testing.T) {
					t.Parallel()

					gate := filepath.Join(t.TempDir(), "release-review")
					script := `while [ ! -f '` + gate + `' ]; do /bin/sleep 0.02; done
` + testkit.ApprovedReview
					if mode == "restore pending" {
						script = `printf tamper > feature.txt
` + testkit.ApprovedReview
					}
					s, initial, api, _, cfg, _ := testkit.TmuxPairingFlow(t, implementer, reviewer, testkit.DisputedReport)
					testkit.ReplacePhaseScript(t, cfg, workflow.Review, testkit.ReviewScript(script))
					cfg.Repositories[0].Reviewer.MaxAttempts = 1
					run := testkit.Finish(t, s, workflow.Active, workflow.Review)
					root := initial.Workspace.Root
					if err := initial.Close(); err != nil {
						t.Fatal(err)
					}
					socket := fmt.Sprintf("mergeyard-review-restart-%d", time.Now().UnixNano())
					t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
					input, _ := json.Marshal(testkit.ReviewProcessInput{Config: cfg, Workspace: root, Socket: socket, PR: api.Prs["mergeyard/issue-7"], KillDuringRestore: mode == "restore pending"})
					path := filepath.Join(t.TempDir(), "input.json")
					if err := os.WriteFile(path, input, 0600); err != nil {
						t.Fatal(err)
					}
					executable, err := os.Executable()
					if err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					defer cancel()
					cmd := exec.CommandContext(ctx, executable, "-test.run=^TestReviewControlPlaneProcess$")
					cmd.Env = append(os.Environ(), "MERGEYARD_TEST_REVIEW_PROCESS="+path)
					cmd.Stderr = os.Stderr
					stdout, err := cmd.StdoutPipe()
					if err != nil {
						t.Fatal(err)
					}
					if err := cmd.Start(); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
					scanner := bufio.NewScanner(stdout)
					if !scanner.Scan() || scanner.Text() != "review-control-plane-ready" {
						t.Fatalf("subprocess not ready: %q %v", scanner.Text(), scanner.Err())
					}
					if err := cmd.Process.Kill(); err != nil {
						t.Fatal(err)
					}
					cmd.Wait()
					if mode == "finished offline" {
						if err := os.WriteFile(gate, nil, 0600); err != nil {
							t.Fatal(err)
						}
						testkit.WaitFor(t, func() bool {
							_, err := os.Stat(filepath.Join(root, "runs", run.ID, "phases", "review-1-1", "exit.json"))
							return err == nil
						})
					}
					runtime, err := app.Open(context.Background(), root)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { runtime.Close() })
					restarted, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: testkit.NewLocalRunner(t, runner.Options{SocketName: socket})})
					if err != nil {
						t.Fatal(err)
					}
					if mode == "still running" {
						for range 2 {
							if _, err := restarted.Reconcile(context.Background()); err != nil {
								t.Fatal(err)
							}
						}
						current, err := runtime.Workflow.Get(context.Background(), run.ID)
						if err != nil || current.Review.Attempt != 1 || current.Review.Report != nil {
							t.Fatalf("running recovery=%+v %v", current, err)
						}
						if err := os.WriteFile(gate, nil, 0600); err != nil {
							t.Fatal(err)
						}
					}
					targetState := workflow.WaitingForCI
					if mode == "restore pending" {
						targetState = workflow.NeedsAttention
					}
					recovered := testkit.Finish(t, restarted, targetState, workflow.Review)
					var attempts int
					runtime.DB.QueryRow("SELECT count(*) FROM review_attempts").Scan(&attempts)
					if attempts != 1 || recovered.Review.Attempt != 1 || !recovered.Review.Restored {
						t.Fatalf("duplicate or lost review recovery=%+v %+v attempts=%d", recovered, recovered.Review, attempts)
					}
					if mode == "restore pending" && (recovered.ApprovedSHA != "" || recovered.Review.Accepted || recovered.LastErrorCode != "review.code_changed") {
						t.Fatal("pending restore accepted contaminated verdict")
					}
				})
			}
		}
	}
}
