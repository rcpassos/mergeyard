package integration_test

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
	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestTmuxFixSurvivesControlPlaneKill(t *testing.T) {
	t.Parallel()

	for _, implementer := range []string{"claude"} {
		for _, reviewer := range []string{"codex"} {
			for _, scenario := range []string{"running", "commit", "push"} {
				t.Run(implementer+"/"+reviewer+"/"+scenario, func(t *testing.T) {
					t.Parallel()

					ciRepair := strings.HasPrefix(scenario, "CI ")
					mode := strings.TrimPrefix(scenario, "CI ")
					finishedOffline := mode == "finished offline" || mode == "finished offline with harness change"
					gate := filepath.Join(t.TempDir(), "release-fix")
					fix := `while [ ! -f '` + gate + `' ]; do /bin/sleep 0.02; done
 printf fixed > feature.txt
 ` + testkit.FixedReport
					if mode != "running" && !finishedOffline {
						if err := os.WriteFile(gate, nil, 0600); err != nil {
							t.Fatal(err)
						}
					}
					s, initial, api, remote, cfg, initialRunner := testkit.TmuxPairingFlow(t, implementer, reviewer, fix)
					if ciRepair {
						fix = strings.Replace(fix, testkit.FixedReport, testkit.CiFixedReport, 1)
						testkit.UseCIRepairScript(t, cfg, fix)
						boundary := &testkit.CiGitHub{FakeGitHub: api, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "failure", Excerpt: "compiler error", URL: "https://example.com/build"}}}}
						var err error
						s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(initial), scheduler.Dependencies{GitHub: testkit.CiBranchGitHub{CiGitHub: boundary}, Runner: initialRunner})
						if err != nil {
							t.Fatal(err)
						}
						testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
					}
					run := testkit.Finish(t, s, workflow.Active, workflow.Fix)
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
					data, _ := json.Marshal(testkit.ReviewProcessInput{Config: cfg, Workspace: root, Socket: socket, PR: api.Prs["mergeyard/issue-7"], KillDuringFix: stage, Remote: remote})
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
						testkit.WaitFor(t, func() bool {
							_, err := os.Stat(filepath.Join(root, "runs", run.ID, "phases", "fix-1-1", "exit.json"))
							return err == nil
						})
					}
					if mode == "finished offline with harness change" {
						cfg.Repositories[0].Implementer.Agent = "codex"
						if implementer == "codex" {
							cfg.Repositories[0].Implementer.Agent = "claude"
						}
					}
					runtime, err := app.Open(context.Background(), root)
					if err != nil {
						t.Fatal(err)
					}
					defer runtime.Close()
					restarted, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: runner.NewLocal(runner.Options{SocketName: socket})})
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
					recovered := testkit.Finish(t, restarted, workflow.WaitingForCI, workflow.Review)
					if recovered.ReviewRound != 2 || recovered.Fix.Attempt != 1 || !recovered.Fix.Pushed || recovered.Fix.Agent != implementer || recovered.Fix.Status != "succeeded" {
						t.Fatalf("recovery=%+v", recovered)
					}
					if ciRepair {
						if recovered.Fix.CI == nil || recovered.Fix.CI.RepairCause != "ci.check_failed" || recovered.Fix.CI.Evidence.Checks[0].Excerpt != "compiler error" || recovered.ApprovedSHA == recovered.Fix.TargetSHA {
							t.Fatal("CI repair context or new approval lost on restart")
						}
						boundary := &testkit.CiGitHub{FakeGitHub: api, ApplyReady: true, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "success"}}}}
						restarted, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.CiBranchGitHub{CiGitHub: boundary}, Runner: runner.NewLocal(runner.Options{SocketName: socket})})
						if err != nil {
							t.Fatal(err)
						}
						ready := testkit.Finish(t, restarted, workflow.ReadyToMerge, workflow.Review)
						if ready.ReviewRound != 2 || boundary.ReadyCalls != 1 {
							t.Fatal("repair not ready after restart and independent review")
						}
					}
					if count := testkit.GitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7"); count != "2" {
						t.Fatalf("duplicate commits=%s", count)
					}
					var attempts int
					runtime.DB.QueryRow("SELECT count(*) FROM phase_attempts").Scan(&attempts)
					if attempts != 4 || api.Creations != 1 {
						t.Fatalf("attempts=%d creations=%d", attempts, api.Creations)
					}
				})
			}
		}
	}
}
