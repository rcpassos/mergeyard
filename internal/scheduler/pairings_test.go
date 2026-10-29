package scheduler_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestHarnessPairingsNoChangeDispute(t *testing.T) {
	t.Parallel()

	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			t.Run(implementer+"/"+reviewer, func(t *testing.T) {
				t.Parallel()

				flow := testkit.NewPairingScenario(t, implementer, reviewer, testkit.DisputedReport)
				s, _, _, _, _, _ := flow.Parts()
				run := flow.Until(s, workflow.WaitingForCI, workflow.Review)
				if run.ReviewRound != 2 || !run.Review.Accepted || run.Fix.Report.Responses[0].Resolution != "disputed" || fmt.Sprint(flow.Git.Sequence-1) != "1" {
					t.Fatalf("no-change dispute failed: %+v", run)
				}
			})
		}
	}
}

func TestHarnessPairingsRejectInvalidReports(t *testing.T) {
	t.Parallel()

	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			for _, phase := range []workflow.Phase{workflow.Review, workflow.Fix} {
				t.Run(implementer+"/"+reviewer+"/"+string(phase), func(t *testing.T) {
					t.Parallel()

					flow := testkit.NewPairingScenario(t, implementer, reviewer, testkit.FixedReport)
					s, _, api, _, _, _ := flow.Parts()
					invalid := strings.ReplaceAll(testkit.ChangesReview, `"changes_required"`, `"approved"`)
					if phase == workflow.Fix {
						flow.Runner.Outcomes["fix"] = []testkit.ScenarioOutcome{testkit.NativeOutcome(strings.ReplaceAll(testkit.FixedReport, `"F1"`, `"unknown"`))}
					} else {
						flow.Runner.Outcomes["review"] = []testkit.ScenarioOutcome{testkit.NativeOutcome(invalid)}
					}

					run := flow.Until(s, workflow.NeedsAttention, phase)
					if run.LastErrorCode != "phase.result_invalid" || run.ReviewRound != 1 || run.ApprovedSHA != "" || api.Creations != 1 || fmt.Sprint(flow.Git.Sequence-1) != "1" {
						t.Fatalf("accepted invalid result: %+v", run)
					}
				})
			}
		}
	}
}

func TestReviewStartupFailureHonorsRoleAttemptLimit(t *testing.T) {
	t.Parallel()

	for _, reviewer := range []string{"claude", "codex"} {
		t.Run(reviewer, func(t *testing.T) {
			t.Parallel()

			flow := testkit.NewPairingScenario(t, "claude", reviewer, testkit.DisputedReport)
			_, _, api, _, cfg, r := flow.Parts()
			cfg.Repositories[0].Reviewer.MaxAttempts = 2
			launches := 0
			failed := testkit.FailReviewLaunch{Runner: r, Observe: func() { launches++ }}
			s, err := flow.New(cfg, scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: failed})
			if err != nil {
				t.Fatal(err)
			}
			run := flow.Until(s, workflow.NeedsAttention, workflow.Review)
			if launches != 2 || run.Review.Attempt != 2 || run.Review.Status != "failed" || !run.Review.Restored {
				t.Fatalf("unbounded or incomplete launch failure: launches=%d %+v", launches, run.Review)
			}
		})
	}
}
