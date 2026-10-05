package scheduler_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/rcpassos/mergeyard/internal/app"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

type interruptedFixGit struct {
	*managedgit.Manager
	stage string
}

func (g interruptedFixGit) CommitFix(ctx context.Context, run managedgit.Run, phase managedgit.Phase, target string) (managedgit.CommitResult, error) {
	result, err := g.Manager.CommitFix(ctx, run, phase, target)
	if err == nil && g.stage == "commit" {
		fmt.Println("review-control-plane-ready")
		select {}
	}
	return result, err
}
func (g interruptedFixGit) PushFix(ctx context.Context, run managedgit.Run, previous, target string) error {
	err := g.Manager.PushFix(ctx, run, previous, target)
	if err == nil && g.stage == "push" {
		fmt.Println("review-control-plane-ready")
		select {}
	}
	return err
}

func TestFixRecoversAfterControlPlaneKill(t *testing.T) {
	for _, mode := range []string{"running", "finished offline", "finished offline with harness change", "commit", "push", "next review"} {
		t.Run(mode, func(t *testing.T) {
			finishedOffline := mode == "finished offline" || mode == "finished offline with harness change"
			gate := filepath.Join(t.TempDir(), "release-fix")
			fix := `while [ ! -f '` + gate + `' ]; do /bin/sleep 0.02; done
 printf fixed > feature.txt
 ` + fixedReport
			if mode != "running" && !finishedOffline {
				if err := os.WriteFile(gate, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			s, initial, api, remote, cfg, _ := localFlow(t, loopScript(fix))
			run := finish(t, s, workflow.Active, workflow.Fix)
			root := initial.Workspace.Root
			if err := initial.Close(); err != nil {
				t.Fatal(err)
			}
			socket := fmt.Sprintf("mergeyard-fix-restart-%d", time.Now().UnixNano())
			t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
			stage := ""
			if mode == "commit" || mode == "push" || mode == "next review" {
				stage = mode
			}
			data, _ := json.Marshal(reviewProcessInput{Config: cfg, Workspace: root, Socket: socket, PR: api.prs["mergeyard/issue-7"], KillDuringFix: stage, Remote: remote})
			path := filepath.Join(t.TempDir(), "input.json")
			if err := os.WriteFile(path, data, 0600); err != nil {
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
				t.Fatalf("not ready: %q %v", scanner.Text(), scanner.Err())
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			cmd.Wait()
			if finishedOffline {
				if err := os.WriteFile(gate, nil, 0600); err != nil {
					t.Fatal(err)
				}
				waitFor(t, func() bool {
					_, err := os.Stat(filepath.Join(root, "runs", run.ID, "phases", "fix-1-1", "exit.json"))
					return err == nil
				})
			}
			if mode == "finished offline with harness change" {
				cfg.Repositories[0].Implementer.Agent = "codex"
			}
			runtime, err := app.Open(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			restarted, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: runner.NewLocal(runner.Options{SocketName: socket})})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "running" {
				for range 2 {
					if _, err := restarted.Reconcile(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				current, err := runtime.Workflow.Get(context.Background(), run.ID)
				if err != nil || current.Fix == nil || current.Fix.Attempt != 1 || current.Fix.Report != nil {
					t.Fatalf("restarted=%+v %v", current, err)
				}
				if err := os.WriteFile(gate, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			recovered := finish(t, restarted, workflow.WaitingForCI, workflow.Review)
			if recovered.ReviewRound != 2 || recovered.Fix.Attempt != 1 || !recovered.Fix.Pushed || recovered.Fix.Agent != "claude" || recovered.Fix.Status != "succeeded" {
				t.Fatalf("recovery=%+v", recovered)
			}
			if count := gitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7"); count != "2" {
				t.Fatalf("duplicate commits=%s", count)
			}
			var attempts int
			runtime.DB.QueryRow("SELECT count(*) FROM phase_attempts").Scan(&attempts)
			if attempts != 4 || api.creations != 1 {
				t.Fatalf("attempts=%d creations=%d", attempts, api.creations)
			}
		})
	}
}
