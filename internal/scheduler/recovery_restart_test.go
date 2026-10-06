package scheduler_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type recoveryCrashRunner struct {
	runner.Runner
	phase workflow.Phase
	mode  string
}

func (r recoveryCrashRunner) StartSession(ctx context.Context, req runner.SessionRequest) (runner.SessionRef, error) {
	replacement := req.Phase == string(r.phase) && req.Attempt == 2
	if replacement && r.mode == "before-launch" {
		fmt.Println("review-control-plane-ready")
		select {}
	}
	ref, err := r.Runner.StartSession(ctx, req)
	if err == nil && replacement && r.mode == "after-launch" {
		fmt.Println("review-control-plane-ready")
		select {}
	}
	return ref, err
}

func TestMissingSessionRecoverySurvivesControlPlaneKill(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		for _, phase := range []workflow.Phase{workflow.Implement, workflow.Review, workflow.Fix} {
			for _, mode := range []string{"before-launch", "after-launch", "after-identity"} {
				t.Run(agent+"/"+string(phase)+"/"+mode, func(t *testing.T) {
					s, initial, api, remote, cfg, _ := recoveryFlow(t, agent, phase, "success")
					gate := filepath.Join(t.TempDir(), "release")
					if mode == "after-identity" {
						executable := cfg.Agents.Claude.Executable
						round := map[workflow.Phase]int{workflow.Implement: 0, workflow.Review: 2, workflow.Fix: 1}[phase]
						second := fmt.Sprintf("%s-%d-2", phase, round)
						held := `case "$*" in *` + second + `*) while [ ! -f '` + gate + `' ]; do /bin/sleep 0.02; done;; esac
`
						if agent == "codex" {
							executable = cfg.Agents.Codex.Executable
						}
						data, err := os.ReadFile(executable)
						if err != nil {
							t.Fatal(err)
						}
						text := string(data)
						if agent == "codex" {
							marker := ` printf '%s\n' "{\"type\":\"thread.started\",\"thread_id\":\"$identity\"}"`
							text = strings.Replace(text, marker, marker+"\n"+held, 1)
						} else {
							text = strings.Replace(text, "#!/bin/sh\n", "#!/bin/sh\n"+held, 1)
						}
						if err := os.WriteFile(executable, []byte(text), 0700); err != nil {
							t.Fatal(err)
						}
					}
					var run workflow.Run
					// Halt at the failed resume, before the next tick reserves recovery.
					waitFor(t, func() bool {
						if err := s.Tick(context.Background()); err != nil {
							t.Fatal(err)
						}
						runs, err := s.Runs(context.Background())
						if err != nil {
							t.Fatal(err)
						}
						if len(runs) != 1 || runs[0].Phase != phase {
							return false
						}
						run = runs[0]
						failure := ""
						switch phase {
						case workflow.Implement:
							if run.Implementer != nil {
								failure = run.Implementer.Error
							}
						case workflow.Review:
							if run.Review != nil {
								failure = run.Review.Error
							}
						case workflow.Fix:
							if run.Fix != nil {
								failure = run.Fix.Error
							}
						}
						return run.State == workflow.Active && strings.HasPrefix(failure, "harness.session_resume_failed:")
					})
					root := initial.Workspace.Root
					if err := initial.Close(); err != nil {
						t.Fatal(err)
					}
					socket := fmt.Sprintf("mergeyard-recovery-kill-%d", time.Now().UnixNano())
					t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
					input, _ := json.Marshal(reviewProcessInput{Config: cfg, Workspace: root, Socket: socket, PR: api.prs["mergeyard/issue-7"], Remote: remote, RecoveryPhase: phase, RecoveryCrash: mode})
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
					out, err := cmd.StdoutPipe()
					if err != nil {
						t.Fatal(err)
					}
					if err := cmd.Start(); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
					scanner := bufio.NewScanner(out)
					if !scanner.Scan() || scanner.Text() != "review-control-plane-ready" {
						t.Fatalf("crash boundary unavailable: %q %v", scanner.Text(), scanner.Err())
					}
					if err := cmd.Process.Kill(); err != nil {
						t.Fatal(err)
					}
					cmd.Wait()
					if mode == "after-identity" {
						if err := os.WriteFile(gate, []byte("release"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					runtime, err := app.Open(context.Background(), root)
					if err != nil {
						t.Fatal(err)
					}
					defer runtime.Close()
					persisted, err := runtime.Workflow.Get(context.Background(), run.ID)
					if err != nil || len(persisted.SessionRecoveries) != 1 {
						t.Fatalf("allowance not persisted before launch: %+v %v", persisted, err)
					}
					if mode == "after-identity" && persisted.SessionRecoveries[0].SessionID == "" {
						t.Fatal("identity not persisted")
					}
					restarted, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: runner.NewLocal(runner.Options{SocketName: socket})})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := restarted.Reconcile(context.Background()); err != nil {
						t.Fatal(err)
					}
					state := workflow.WaitingForCI
					finalPhase := workflow.Review
					if mode == "before-launch" {
						state, finalPhase = workflow.NeedsAttention, phase
					} else if phase == workflow.Implement {
						state = workflow.Active
					}
					saved := finish(t, restarted, state, finalPhase)
					if len(saved.SessionRecoveries) != 1 {
						t.Fatal("recovery replenished after crash")
					}
					if mode == "before-launch" && saved.LastErrorCode != "phase.session_missing" {
						t.Fatalf("ambiguous launch replayed: %+v", saved)
					}
					var attempts int
					if err := runtime.DB.QueryRow("SELECT count(*) FROM phase_attempts WHERE run_id=? AND phase=? AND round=?", run.ID, phase, map[workflow.Phase]int{workflow.Implement: 0, workflow.Fix: 1, workflow.Review: 2}[phase]).Scan(&attempts); err != nil {
						t.Fatal(err)
					}
					if attempts != 2 {
						t.Fatalf("duplicate fresh attempt: %d", attempts)
					}
				})
			}
		}
	}
}
