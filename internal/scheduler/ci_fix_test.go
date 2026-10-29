package scheduler_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestCIFixRequiresAvailableReviewRound(t *testing.T) {
	t.Parallel()

	for _, max := range []int{1, 2} {
		t.Run(string(rune('0'+max)), func(t *testing.T) {
			t.Parallel()

			flow := testkit.NewScenario(t)
			_, _, base, _, cfg, r := flow.Parts()
			cfg.MaxRounds = max
			api := &testkit.CiGitHub{FakeGitHub: base, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "timed_out"}}}}
			s, err := flow.New(cfg, scheduler.Dependencies{GitHub: testkit.CiBranchGitHub{CiGitHub: api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			flow.Until(s, workflow.WaitingForCI, workflow.Review)
			run := flow.Until(s, workflow.NeedsAttention, workflow.Review)
			if run.LastErrorCode != "review.max_rounds_exceeded" || run.ReviewRound != max || len(run.FixHistory) != max-1 || api.ReadyCalls != 0 || run.CI.Evidence.Checks[0].Conclusion != "timed_out" {
				t.Fatalf("unreviewable repair launched: %+v", run)
			}
		})
	}
}

func TestCINonCodeOutcomesNeverLaunchRepair(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"cancellation with failure", "action with timeout", "wait deadline", "unreadable failed evidence", "expired failure", "expired terminal timeout", "failure query crosses deadline", "PR reread crosses deadline"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			flow := testkit.NewScenario(t)
			_, runtime, base, _, cfg, r := flow.Parts()
			now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
			api := &testkit.CiGitHub{FakeGitHub: base, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "failure"}}}}
			delayRead := false
			boundary := testkit.CiDeadlineGitHub{CiGitHub: api, BeforeRead: func() {
				if delayRead {
					now = now.Add(time.Hour)
					delayRead = false
				}
			}}
			s, err := flow.New(cfg, scheduler.Dependencies{GitHub: boundary, Runner: r, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			run := flow.Until(s, workflow.WaitingForCI, workflow.Review)
			code := "ci.action_required"
			switch mode {
			case "cancellation with failure":
				api.Evidence.Checks = append(api.Evidence.Checks, ci.Check{Name: "canceled", Source: "check", Status: "completed", Conclusion: "cancelled"})
			case "action with timeout":
				api.Evidence.Checks = []ci.Check{{Name: "action", Source: "check", Status: "completed", Conclusion: "action_required"}, {Name: "timeout", Source: "check", Status: "completed", Conclusion: "timed_out"}}
			case "wait deadline":
				api.Evidence.Checks[0].Status = "queued"
				api.Evidence.Checks[0].Conclusion = ""
				now = now.Add(time.Hour)
				code = "ci.wait_timeout"
			case "unreadable failed evidence":
				api.QueryError = os.ErrPermission
				code = "ci.wait_timeout"
			case "expired failure", "expired terminal timeout":
				now = now.Add(time.Hour)
				code = "ci.wait_timeout"
				if mode == "expired terminal timeout" {
					api.Evidence.Checks[0].Conclusion = "timed_out"
				}
			case "failure query crosses deadline":
				api.AfterEvidence = func() { now = now.Add(time.Hour) }
				code = "ci.wait_timeout"
			case "PR reread crosses deadline":
				api.AfterEvidence = func() { delayRead = true }
				code = "ci.wait_timeout"
			}
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if mode == "unreadable failed evidence" {
				waiting, _ := runtime.Workflow.Get(context.Background(), run.ID)
				if waiting.State != workflow.WaitingForCI || waiting.CI.QueryError == "" || !waiting.CI.Deadline.Equal(run.CI.Deadline) {
					t.Fatal("unreadable failure must retain bounded wait")
				}
				now = now.Add(time.Hour)
			}
			for range 2 {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			got, _ := runtime.Workflow.Get(context.Background(), run.ID)
			if got.State != workflow.NeedsAttention || got.LastErrorCode != code || len(got.FixHistory) != 0 || api.ReadyCalls != 0 {
				t.Fatalf("non-code CI outcome launched repair: %+v", got)
			}
		})
	}
}
