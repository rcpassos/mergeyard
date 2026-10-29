package scheduler_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"

	"github.com/rcpassos/mergeyard/internal/fault"
)

func TestExplicitRetryRenewsCIWithoutAgentOrRound(t *testing.T) {
	t.Parallel()

	flow := testkit.NewScenario(t)
	_, runtime, base, _, cfg, r := flow.Parts()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	api := &testkit.CiGitHub{FakeGitHub: base, ApplyReady: true, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "queued"}}}}
	deps := scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }}
	s, err := flow.New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	before := flow.Until(s, workflow.WaitingForCI, workflow.Review)
	now = before.CI.Deadline
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	selected, err := s.Retry(context.Background(), before.ID)
	if err != nil || selected.State != workflow.WaitingForCI || selected.ApprovedSHA != before.ApprovedSHA || !selected.CI.Deadline.Equal(now.Add(cfg.CITimeout)) {
		t.Fatalf("renewal: %+v %v", selected, err)
	}
	if testkit.Has(base.Issues["owner/repo"][0], "agent-needs-attention") || !testkit.Has(base.Issues["owner/repo"][0], "agent-running") {
		t.Fatal("CI retry did not reconcile progress labels")
	}
	deadline := selected.CI.Deadline
	if _, err := s.Retry(context.Background(), before.ID); err != nil {
		t.Fatal(err)
	}
	s, err = flow.New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := runtime.Workflow.Get(context.Background(), before.ID)
	if err != nil || !after.CI.Deadline.Equal(deadline) || after.ReviewRound != before.ReviewRound {
		t.Fatalf("restart: %+v %v", after, err)
	}
	var attempts int
	if err := runtime.DB.QueryRow("SELECT count(*) FROM phase_attempts WHERE run_id=?", after.ID).Scan(&attempts); err != nil || attempts != 2 {
		t.Fatalf("CI spent attempt: %d %v", attempts, err)
	}
}

func TestExplicitRetryCIOutcomes(t *testing.T) {
	t.Parallel()

	for _, outcome := range []string{"success", "pending", "unknown", "failure", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()

			flow := testkit.NewScenario(t)
			_, runtime, base, _, cfg, r := flow.Parts()
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			api := &testkit.CiGitHub{FakeGitHub: base, ApplyReady: true, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "queued"}}}}
			s, err := flow.New(cfg, scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			before := flow.Until(s, workflow.WaitingForCI, workflow.Review)
			now = before.CI.Deadline
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := workflow.WaitingForCI
			switch outcome {
			case "success":
				api.Evidence.Checks[0].Status = "completed"
				api.Evidence.Checks[0].Conclusion = "success"
				want = workflow.ReadyToMerge
			case "unknown":
				api.QueryError = errors.New("CI temporarily unavailable")
			case "failure":
				api.Evidence.Checks[0].Status = "completed"
				api.Evidence.Checks[0].Conclusion = "failure"
				want = workflow.Active
			case "cancelled":
				api.Evidence.Checks[0].Status = "completed"
				api.Evidence.Checks[0].Conclusion = "cancelled"
				want = workflow.NeedsAttention
			}
			selected, err := s.Retry(context.Background(), before.ID)
			if err != nil || selected.State != want || selected.ReviewRound != before.ReviewRound || selected.Retries[0].GrantedRound != 0 {
				t.Fatalf("CI retry: %+v %v", selected, err)
			}
			if want != workflow.Active && selected.ApprovedSHA != before.ApprovedSHA {
				t.Fatal("lost current approval")
			}
			if want == workflow.Active && (selected.Phase != workflow.Fix || selected.ApprovedSHA != "") {
				t.Fatal("repair retained approval")
			}
			var attempts int
			if err := runtime.DB.QueryRow("SELECT count(*) FROM phase_attempts WHERE run_id=?", before.ID).Scan(&attempts); err != nil || attempts != 2 {
				t.Fatalf("CI retry launched work: %d %v", attempts, err)
			}
		})
	}
}

func TestExplicitRetryRecognizesCompletedImplementation(t *testing.T) {
	t.Parallel()

	flow := testkit.NewScenario(t)
	_, _, base, _, cfg, r := flow.Parts()
	api := &testkit.RetryPublicationGitHub{FakeGitHub: base, Unavailable: true}
	s, err := flow.New(cfg, scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	before := flow.Until(s, workflow.NeedsAttention, workflow.Implement)
	if before.Implementer.Status != "succeeded" {
		t.Fatalf("fixture: %+v", before)
	}
	api.Unavailable = false
	selected, err := s.Retry(context.Background(), before.ID)
	if err != nil || selected.Retries[0].AttemptFrom != 0 {
		t.Fatalf("completed work authorized another attempt: %+v %v", selected, err)
	}
	after := flow.Until(s, workflow.WaitingForCI, workflow.Review)
	if after.ID != before.ID || after.Implementer.Attempt != 1 || api.Creations != 1 {
		t.Fatalf("replayed completed work: %+v", after)
	}
}

func TestExplicitRetryCILabelFailureDoesNotBypassDeadline(t *testing.T) {
	t.Parallel()

	flow := testkit.NewScenario(t)
	_, runtime, base, _, cfg, r := flow.Parts()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	api := &testkit.CiGitHub{FakeGitHub: base, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "queued"}}}}
	deps := scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }}
	s, err := flow.New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	before := flow.Until(s, workflow.WaitingForCI, workflow.Review)
	now = before.CI.Deadline
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	api.Mutate = func(action, repo string, n int, label string) error {
		if action == "add" && label == "agent-running" {
			return errors.New("GitHub labels unavailable")
		}
		return nil
	}
	selected, err := s.Retry(context.Background(), before.ID)
	if err == nil || selected.State != workflow.WaitingForCI || selected.Retries[0].Pending {
		t.Fatalf("label failure lost selection: %+v %v", selected, err)
	}
	s, err = flow.New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	now = selected.CI.Deadline
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := runtime.Workflow.Get(context.Background(), before.ID)
	if err != nil || after.State != workflow.NeedsAttention || after.LastErrorCode != "ci.wait_timeout" || len(after.Retries) != 1 || after.ReviewRound != before.ReviewRound {
		t.Fatalf("labels bypassed deadline: %+v %v", after, err)
	}
}

func TestExplicitRetryDoesNotReplenishMissingSessionRecovery(t *testing.T) {
	t.Parallel()

	for _, agent := range []string{"claude", "codex"} {
		for _, phase := range []workflow.Phase{workflow.Implement, workflow.Review, workflow.Fix} {
			t.Run(agent+"/"+string(phase), func(t *testing.T) {
				flow := testkit.NewRecoveryScenario(t, agent, phase, "exhausted")
				s, _, api, _, cfg, r := flow.Parts()
				before := flow.Until(s, workflow.NeedsAttention, phase)
				if before.LastErrorCode != "harness.session_resume_failed" || len(before.SessionRecoveries) != 1 {
					t.Fatalf("fixture: %+v", before)
				}
				flow.Reopen()
				restarted := flow.Runtime
				var err error
				s, err = flow.New(cfg, scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
				if err != nil {
					t.Fatal(err)
				}

				for range 2 {
					got, err := s.Retry(context.Background(), before.ID)
					var coded *fault.Error
					if !errors.As(err, &coded) || coded.Code != "harness.session_resume_failed" || got.State != workflow.NeedsAttention || len(got.SessionRecoveries) != 1 {
						t.Fatalf("replenished recovery: %+v %v", got, err)
					}
				}
				var count int
				if err := restarted.DB.QueryRow("SELECT count(*) FROM phase_attempts WHERE run_id=? AND phase=?", before.ID, phase).Scan(&count); err != nil || count != map[workflow.Phase]int{workflow.Implement: 3, workflow.Fix: 3, workflow.Review: 4}[phase] {
					t.Fatalf("recovery spent extra attempt: %d %v", count, err)
				}
			})
		}
	}
}
