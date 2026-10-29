package scheduler_test

import (
	"context"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestFixRecoversAcrossDurableBoundaries(t *testing.T) {
	t.Parallel()
	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			for _, boundary := range []string{"running", "finished offline", "finished offline with harness change", "commit", "push", "next review", "CI commit", "CI push", "CI next review"} {
				t.Run(implementer+"/"+reviewer+"/"+boundary, func(t *testing.T) {
					t.Parallel()
					flow := testkit.NewPairingScenario(t, implementer, reviewer, testkit.FixedReport)
					cfg, api := flow.Cfg, flow.Api
					ciRepair := strings.HasPrefix(boundary, "CI ")
					mode := strings.TrimPrefix(boundary, "CI ")
					offline := strings.HasPrefix(mode, "finished offline")
					hold := mode == "running" || offline
					flow.Runner.Outcomes["fix"][0].Hold = hold
					deps := scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}}
					if ciRepair {
						flow.Runner.Outcomes["review"] = nil
						result := testkit.NativeOutcome(testkit.CiFixedReport)
						result.Apply = testkit.ScenarioWrite(t, "feature.txt", "fixed")
						flow.Runner.Outcomes["fix"] = []testkit.ScenarioOutcome{result}
						deps.GitHub = testkit.CiBranchGitHub{CiGitHub: &testkit.CiGitHub{FakeGitHub: api, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "failure", Excerpt: "compiler error", URL: "https://example.com/build"}}}}}
					}
					s, err := flow.New(cfg, deps)
					if err != nil {
						t.Fatal(err)
					}
					if ciRepair {
						flow.Until(s, workflow.WaitingForCI, workflow.Review)
					}
					run := flow.Until(s, workflow.Active, workflow.Fix)
					if mode == "commit" || mode == "push" {
						flow.Git.After = func(stage string) {
							if stage == mode {
								panic(testkit.ScenarioCrash{})
							}
						}
						crashed := false
						for range 20 {
							if testkit.CrashTick(t, s) {
								crashed = true
								break
							}
						}
						if !crashed {
							t.Fatal("crash boundary not reached")
						}
						flow.Git.After = nil
					} else if mode == "next review" {
						flow.Until(s, workflow.Active, workflow.Review)
					} else {
						if err := s.Tick(t.Context()); err != nil {
							t.Fatal(err)
						} // Start the held fix.
					}
					// Reopening the actual store loses controller-local state. The runner's
					// independent session map and Git refs are the surviving external world.
					flow.Reopen()
					if offline {
						ref := flow.Runner.Requests[len(flow.Runner.Requests)-1]
						flow.Runner.Complete(testkit.RunnerRef(ref))
					}
					if mode == "finished offline with harness change" {
						cfg.Repositories[0].Implementer.Agent = "codex"
						if implementer == "codex" {
							cfg.Repositories[0].Implementer.Agent = "claude"
						}
					}
					restarted, err := flow.New(cfg, scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}})
					if err != nil {
						t.Fatal(err)
					}
					if mode == "running" {
						for range 2 {
							if _, err := restarted.Reconcile(t.Context()); err != nil {
								t.Fatal(err)
							}
						}
						current, err := flow.Runtime.Workflow.Get(t.Context(), run.ID)
						if err != nil || current.Fix == nil || current.Fix.Attempt != 1 || current.Fix.Report != nil {
							t.Fatalf("restarted=%+v %v", current, err)
						}
						req := flow.Runner.Requests[len(flow.Runner.Requests)-1]
						flow.Runner.Complete(testkit.RunnerRef(req))
					}
					recovered := flow.Until(restarted, workflow.WaitingForCI, workflow.Review)
					if recovered.ReviewRound != 2 || recovered.Fix.Attempt != 1 || !recovered.Fix.Pushed || recovered.Fix.Agent != implementer || recovered.Fix.Status != "succeeded" {
						t.Fatalf("recovery=%+v", recovered)
					}
					if ciRepair {
						if recovered.Fix.CI == nil || recovered.Fix.CI.RepairCause != "ci.check_failed" || recovered.Fix.CI.Evidence.Checks[0].Excerpt != "compiler error" || recovered.ApprovedSHA == recovered.Fix.TargetSHA {
							t.Fatal("CI context or new approval lost")
						}
						api := &testkit.CiGitHub{FakeGitHub: api, ApplyReady: true, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "success"}}}}
						readyScheduler, err := flow.New(cfg, scheduler.Dependencies{GitHub: testkit.CiBranchGitHub{CiGitHub: api}})
						if err != nil {
							t.Fatal(err)
						}
						ready := flow.Until(readyScheduler, workflow.ReadyToMerge, workflow.Review)
						if ready.ReviewRound != 2 || api.ReadyCalls != 1 {
							t.Fatal("CI repair not independently reviewed")
						}
					}
					if flow.Git.Sequence-1 != 2 {
						t.Fatalf("duplicate commits: %d", flow.Git.Sequence-1)
					}
					var attempts int
					if err := flow.Runtime.DB.QueryRowContext(context.Background(), "SELECT count(*) FROM phase_attempts").Scan(&attempts); err != nil {
						t.Fatal(err)
					}
					if attempts != 4 || api.Creations != 1 {
						t.Fatalf("attempts=%d creations=%d", attempts, api.Creations)
					}
				})
			}
		}
	}
}
