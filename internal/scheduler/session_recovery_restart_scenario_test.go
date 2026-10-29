package scheduler_test

import (
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestMissingSessionRecoverySurvivesDurableBoundaries(t *testing.T) {
	t.Parallel()
	for _, agent := range []string{"claude", "codex"} {
		for _, phase := range []workflow.Phase{workflow.Implement, workflow.Review, workflow.Fix} {
			for _, boundary := range []string{"before-launch", "after-launch", "after-identity"} {
				t.Run(agent+"/"+string(phase)+"/"+boundary, func(t *testing.T) {
					t.Parallel()
					flow := testkit.NewRecoveryScenario(t, agent, phase, "success")
					round := map[workflow.Phase]int{workflow.Implement: 0, workflow.Review: 2, workflow.Fix: 1}[phase]
					if boundary == "after-identity" {
						index := 1
						if phase == workflow.Review {
							index = 2
						}
						flow.Runner.Outcomes[string(phase)][index].Hold = true
					}
					s, _, api, _, cfg, _ := flow.Parts()
					run := flow.Advance(s, func(run workflow.Run) bool {
						if run.Phase != phase || run.State != workflow.Active {
							return false
						}
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
						return strings.HasPrefix(failure, "harness.session_resume_failed:")
					})
					match := func(req runner.SessionRequest) bool {
						return req.Phase == string(phase) && req.Round == round && req.Attempt == 2
					}
					switch boundary {
					case "before-launch":
						flow.Runner.BeforeStart = func(req runner.SessionRequest) {
							if match(req) {
								panic(testkit.ScenarioCrash{})
							}
						}
					case "after-launch":
						flow.Runner.AfterStart = func(req runner.SessionRequest) {
							if match(req) {
								panic(testkit.ScenarioCrash{})
							}
						}
					}
					if boundary == "after-identity" {
						// Write a live native discovery without completing the held execution.
						flow.Runner.AfterStart = func(req runner.SessionRequest) {
							if match(req) {
								if agent == "codex" {
									data := []byte("{\"type\":\"thread.started\",\"thread_id\":\"" + testkit.ReplacementID + "\"}\n")
									if err := flow.Runner.WriteFile(t.Context(), req.PhaseDir+"/events.jsonl", data, 0600); err != nil {
										t.Fatal(err)
									}
								}
							}
						}
						flow.Advance(s, func(run workflow.Run) bool {
							return len(run.SessionRecoveries) == 1 && run.SessionRecoveries[0].SessionID != ""
						})
					} else {
						crashed := false
						for range 20 {
							if testkit.CrashTick(t, s) {
								crashed = true
								break
							}
						}
						if !crashed {
							t.Fatal("crash boundary unavailable")
						}
					}
					flow.Runner.BeforeStart = nil
					flow.Runner.AfterStart = nil
					flow.Reopen()
					persisted, err := flow.Runtime.Workflow.Get(t.Context(), run.ID)
					if err != nil || len(persisted.SessionRecoveries) != 1 {
						t.Fatalf("allowance not persisted: %+v %v", persisted, err)
					}
					if boundary == "after-identity" {
						if persisted.SessionRecoveries[0].SessionID == "" {
							t.Fatal("identity not persisted")
						}
						req := flow.Runner.Requests[len(flow.Runner.Requests)-1]
						flow.Runner.Complete(testkit.RunnerRef(req))
					}
					restarted, err := flow.New(cfg, scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := restarted.Reconcile(t.Context()); err != nil {
						t.Fatal(err)
					}
					state, finalPhase := workflow.WaitingForCI, workflow.Review
					if boundary == "before-launch" {
						state, finalPhase = workflow.NeedsAttention, phase
					} else if phase == workflow.Implement {
						state = workflow.Active
					}
					saved := flow.Until(restarted, state, finalPhase)
					if len(saved.SessionRecoveries) != 1 {
						t.Fatal("recovery replenished after crash")
					}
					if boundary == "before-launch" && saved.LastErrorCode != "phase.session_missing" {
						t.Fatalf("ambiguous launch replayed: %+v", saved)
					}
					var attempts int
					if err := flow.Runtime.DB.QueryRow("SELECT count(*) FROM phase_attempts WHERE run_id=? AND phase=? AND round=?", run.ID, phase, round).Scan(&attempts); err != nil {
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
