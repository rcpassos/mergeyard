package scheduler_test

import (
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestHarnessPairingsRejectInvalidPriorOutcomes(t *testing.T) {
	t.Parallel()

	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			for _, tc := range []struct{ name, report string }{
				{"missing", strings.Replace(testkit.ApprovedAfterFixReview, `{"finding_id":"F1","outcome":"fixed","note":"Source repaired"},`, "", 1)},
				{"unknown", strings.Replace(testkit.ApprovedAfterFixReview, `"finding_id":"F1"`, `"finding_id":"unknown"`, 1)},
				{"not fixed omitted", strings.Replace(testkit.ApprovedAfterFixReview, `"outcome":"fixed"`, `"outcome":"not_fixed"`, 1)},
				{"dispute called fixed", strings.Replace(testkit.ApprovedAfterFixReview, `"outcome":"no_longer_applies"`, `"outcome":"fixed"`, 1)},
			} {
				t.Run(implementer+"/"+reviewer+"/"+tc.name, func(t *testing.T) {
					t.Parallel()

					flow := testkit.NewPairingScenario(t, implementer, reviewer, testkit.FixedReport)
					_, _, api, _, cfg, r := flow.Parts()
					if tc.name == "missing" {
						cfg.Repositories[0].Reviewer.MaxAttempts = 2
					}
					flow.Runner.Outcomes["review"] = []testkit.ScenarioOutcome{testkit.NativeOutcome(testkit.ChangesReview), testkit.NativeOutcome(tc.report), testkit.NativeOutcome(tc.report)}
					s, err := flow.New(cfg, scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
					if err != nil {
						t.Fatal(err)
					}
					run := flow.Until(s, workflow.NeedsAttention, workflow.Review)
					if run.LastErrorCode != "phase.result_invalid" || run.ReviewRound != 2 || run.Review.Accepted || run.ApprovedSHA != "" || run.Review.Attempt != cfg.Repositories[0].Reviewer.MaxAttempts {
						t.Fatalf("invalid prior outcomes bypassed result failure path: %+v", run)
					}
				})
			}
		}
	}
}
