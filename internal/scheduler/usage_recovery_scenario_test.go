package scheduler_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestReviewAndFixUsageLimitsSurviveDurableBoundaries(t *testing.T) {
	t.Parallel()
	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			for _, phase := range []workflow.Phase{workflow.Review, workflow.Fix} {
				boundaries := []string{"interruption", "waiting", "before-relaunch", "resuming"}
				if phase == workflow.Review {
					boundaries = append(boundaries, "before-restoration", "restoration")
				}
				for _, boundary := range boundaries {
					t.Run(implementer+"/"+reviewer+"/"+string(phase)+"/"+boundary, func(t *testing.T) {
						t.Parallel()
						flow := testkit.NewPairingScenario(t, implementer, reviewer, testkit.FixedReport)
						cfg, api := flow.Cfg, flow.Api
						agent := reviewer
						if phase == workflow.Fix {
							agent = implementer
						}
						failed := testkit.ScenarioOutcome{Exit: 1, Apply: testkit.ScenarioWrite(t, "partial.txt", "partial")}
						recovered := testkit.NativeOutcome(testkit.FixedReport)
						recovered.Apply = func(req runner.SessionRequest) {
							partial, err := os.ReadFile(filepath.Join(req.Command.Dir, "partial.txt"))
							if err != nil || string(partial) != "partial" {
								t.Fatalf("interrupted fix work did not survive into attempt %d: %q, %v", req.Attempt, partial, err)
							}
							testkit.ScenarioWrite(t, "feature.txt", "fixed")(req)
						}
						if phase == workflow.Review {
							failed.Apply = testkit.ScenarioWrite(t, "feature.txt", "contaminated")
							recovered = testkit.NativeOutcome(testkit.ApprovedReview)
						}
						flow.Runner.Outcomes[string(phase)] = []testkit.ScenarioOutcome{failed, recovered}
						now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
						classified := testkit.ClassifiedAdapter(agent, harness.NewClaude(cfg.Agents.Claude), harness.NewCodex(cfg.Agents.Codex))
						deps := scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{agent: classified}}
						s, err := flow.New(cfg, deps)
						if err != nil {
							t.Fatal(err)
						}
						run := flow.Until(s, workflow.Active, phase)
						switch boundary {
						case "interruption":
							classified.BeforeClassify = func() { panic(testkit.ScenarioCrash{}) }
						case "before-restoration", "restoration":
							flow.Git.After = func(stage string) {
								if stage == boundary {
									panic(testkit.ScenarioCrash{})
								}
							}
						case "before-relaunch":
							flow.Runner.BeforeStart = func(req runner.SessionRequest) {
								if req.Phase == string(phase) && req.Attempt == 2 {
									panic(testkit.ScenarioCrash{})
								}
							}
						case "resuming":
							flow.Runner.AfterStart = func(req runner.SessionRequest) {
								if req.Phase == string(phase) && req.Attempt == 2 {
									panic(testkit.ScenarioCrash{})
								}
							}
						}
						if boundary == "waiting" || boundary == "before-relaunch" || boundary == "resuming" {
							waiting := flow.Until(s, workflow.WaitingForHarness, phase)
							if boundary != "waiting" {
								now = waiting.HarnessWait.ResetAt
							}
						}
						if boundary != "waiting" {
							crashed := false
							for range 30 {
								if testkit.CrashTick(t, s) {
									crashed = true
									break
								}
							}
							if !crashed {
								t.Fatal("crash boundary not reached")
							}
						}
						classified.BeforeClassify = nil
						flow.Git.After = nil
						flow.Runner.BeforeStart = nil
						flow.Runner.AfterStart = nil
						flow.Reopen()
						restarted, err := flow.New(cfg, deps)
						if err != nil {
							t.Fatal(err)
						}
						if boundary != "resuming" && boundary != "before-relaunch" {
							waiting := flow.Until(restarted, workflow.WaitingForHarness, phase)
							if len(waiting.HarnessWaitHistory) != 1 || waiting.ReviewRound != 1 || (phase == workflow.Review && (!waiting.Review.Restored || !waiting.Review.Contaminated || waiting.Review.Accepted || waiting.Review.Report != nil)) {
								t.Fatalf("interruption/restoration lost: %+v", waiting)
							}
							for range 2 {
								if _, err := restarted.Reconcile(t.Context()); err != nil {
									t.Fatal(err)
								}
							}
							resumed, err := flow.Runtime.Workflow.Get(t.Context(), run.ID)
							if err != nil || resumed.State != workflow.WaitingForHarness || len(resumed.HarnessWaitHistory) != 1 {
								t.Fatalf("waiting advanced early: %+v %v", resumed, err)
							}
							now = waiting.HarnessWait.ResetAt
						} else {
							saved, err := flow.Runtime.Workflow.Get(t.Context(), run.ID)
							if err != nil || saved.HarnessWait == nil {
								t.Fatalf("relaunch lost its wait: %+v %v", saved, err)
							}
							now = saved.HarnessWait.ResetAt
						}
						state, finalPhase := workflow.WaitingForCI, workflow.Review
						if boundary == "before-relaunch" {
							state, finalPhase = workflow.NeedsAttention, phase
						}
						done := flow.Until(restarted, state, finalPhase)
						if len(done.HarnessWaitHistory) != 1 || api.Creations != 1 || len(done.SessionRecoveries) != 0 {
							t.Fatalf("duplicated execution/publication: %+v", done)
						}
						if phase == workflow.Review && (len(done.ReviewHistory) != 2 || done.Review.Attempt != 2 || done.Review.SessionID != done.ReviewHistory[0].SessionID || !done.Review.Restored) {
							t.Fatalf("review continuity lost: %+v", done.ReviewHistory)
						}
						if phase == workflow.Fix && (len(done.FixHistory) != 2 || done.Fix.Attempt != 2 || done.Fix.SessionID != run.Implementer.SessionID || done.Implementer.SessionID != run.Implementer.SessionID) {
							t.Fatalf("fix continuity lost: %+v", done.FixHistory)
						}
						if boundary == "before-relaunch" {
							if done.LastErrorCode != "phase.session_missing" || done.ApprovedSHA != "" || done.ReviewRound != 1 {
								t.Fatalf("ambiguous launch replayed: %+v", done)
							}
						} else if done.ApprovedSHA != api.Head("mergeyard/issue-7") || (phase == workflow.Fix && done.ReviewRound != 2) {
							t.Fatalf("approved wrong target or consumed a round: %+v", done)
						}
						for range 2 {
							if _, err := restarted.Reconcile(t.Context()); err != nil {
								t.Fatal(err)
							}
						}
						if err := restarted.Stop(t.Context(), done.ID); err != nil {
							t.Fatal(err)
						}
					})
				}
			}
		}
	}
}
