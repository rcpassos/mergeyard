package scheduler_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestCreditProbeConcurrentSelectionStopAndWaitingRetry(t *testing.T) {
	t.Parallel()

	flow := testkit.NewScenario(t)
	_, rt, api, _, cfg, r := flow.Parts()
	r.Queue("implement", testkit.ScenarioOutcome{Exit: 1}, testkit.ScenarioOutcome{Exit: 1})
	cfg.Concurrency = 2
	cfg.Repositories[0].Concurrency = 2
	api.Issues["owner/repo"] = append(api.Issues["owner/repo"], testkit.Ready(8))
	h := &testkit.CreditHarness{ClassifiedHarness: &testkit.ClassifiedHarness{HarnessAdapter: harness.NewClaude(cfg.Agents.Claude), Outcomes: []harness.FailureClassification{{Kind: harness.CreditsExhausted}, {Kind: harness.TemporaryLimit}}}, Detection: true}
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	deps := scheduler.Dependencies{Now: func() time.Time { return now }, GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{"claude": h}}
	s, err := flow.New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	var runs []workflow.Run
	testkit.WaitFor(t, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err = s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return len(runs) == 2 && ((runs[0].State == workflow.NeedsAttention && runs[1].State == workflow.WaitingForHarness) || (runs[1].State == workflow.NeedsAttention && runs[0].State == workflow.WaitingForHarness))
	})
	for _, run := range runs {
		if !run.RetryEligible || run.HarnessWait.Reason != "credits_exhausted" || !run.HarnessWait.ResetAt.Equal(now.Add(cfg.UsageLimits.Cooldown)) {
			t.Fatalf("affected run: %+v", run)
		}
	}
	type selection struct {
		run workflow.Run
		Err error
	}
	results := make(chan selection, 2)
	for _, run := range runs {
		go func(id string) { v, err := s.Retry(context.Background(), id); results <- selection{v, err} }(run.ID)
	}
	first, second := <-results, <-results
	if (first.Err == nil) == (second.Err == nil) {
		t.Fatalf("exclusive selection: %v %v", first.Err, second.Err)
	}
	winner := first.run
	if first.Err != nil {
		winner = second.run
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	busy, err := s.Runs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range busy {
		if run.RetryEligible {
			t.Fatalf("busy harness offered Retry: %+v", run)
		}
		page := httptest.NewRecorder()
		server.ServeHTTP(page, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil))
		if page.Code != 200 || strings.Contains(page.Body.String(), ">Retry run<") {
			t.Fatalf("busy probe offered Retry: %d %s", page.Code, page.Body.String())
		}
		if run.ID != winner.ID {
			testkit.AssertCreditConflict(t, rt.Workspace.Root, server, run.ID, "harness.probe_unavailable", "refresh before retrying")
		}
	}
	for range 2 {
		v, err := s.Retry(context.Background(), winner.ID)
		if err != nil || len(v.CreditProbes) != 1 {
			t.Fatalf("duplicate reservation: %+v %v", v, err)
		}
	}
	// Stop before launch releases authorization even if label bookkeeping fails.
	api.Mutate = func(action, repo string, n int, label string) error {
		if action == "remove" {
			return fmt.Errorf("labels unavailable")
		}
		return nil
	}
	if err := s.Stop(context.Background(), winner.ID); err == nil {
		t.Fatal("expected label failure")
	}
	states, err := s.HarnessAvailability(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range states {
		if v.Harness == "claude" && (v.Available || v.ProbeID != "") {
			t.Fatalf("Stop released block instead of reservation: %+v", v)
		}
	}
	api.Mutate = nil
	if err := s.Stop(context.Background(), winner.ID); err != nil {
		t.Fatal(err)
	}
	loser := runs[0]
	if loser.ID == winner.ID {
		loser = runs[1]
	}
	now = now.Add(time.Hour)
	held := testkit.NativeOutcome(testkit.SuccessfulScript)
	held.Hold = true
	r.Queue("implement", held)
	if _, err := s.Retry(context.Background(), loser.ID); err != nil {
		t.Fatal(err)
	}
	testkit.WaitFor(t, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		states, err = s.HarnessAvailability(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range states {
			if v.Harness == "claude" {
				return v.ProbeAttemptID != ""
			}
		}
		return false
	})
	if err := s.Stop(context.Background(), loser.ID); err != nil {
		t.Fatal(err)
	}
	states, err = s.HarnessAvailability(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range states {
		if v.Harness == "claude" && (v.Available || v.ProbeID != "") {
			t.Fatalf("Stop after launch: %+v", v)
		}
	}
}
