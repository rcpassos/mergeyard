package integration_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// Retry asks for another attempt, so the publications that stopped for good
// get the same chance.
func TestRetryRequeuesStoppedPublications(t *testing.T) {
	t.Parallel()

	_, runtime, api, _, cfg, r := testkit.TmuxFlow(t, "exit 1")
	publisher := &testkit.ReportGitHub{BranchGitHub: testkit.BranchGitHub{FakeGitHub: api}}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: publisher, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
	testkit.SeedStoppedReport(t, runtime, run.ID, 101)
	if _, err := s.Retry(context.Background(), run.ID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	var stopped bool
	var code, warning string
	if err := runtime.DB.QueryRow(`SELECT stopped,warning_code,warning FROM report_publications WHERE run_id=?`, run.ID).Scan(&stopped, &code, &warning); err != nil || stopped || code != "" || warning != "" {
		t.Fatalf("retry kept the publication stopped: %v %q %q, %v", stopped, code, warning, err)
	}
	// The requeued publication is attempted again.
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := runtime.DB.QueryRow(`SELECT state FROM report_publications WHERE run_id=?`, run.ID).Scan(&state); err != nil || state != "published" {
		t.Fatalf("requeued publication = %q, %v", state, err)
	}
	reports := 0
	for _, body := range publisher.Bodies {
		if strings.Contains(body, fmt.Sprintf("<!-- mergeyard:report:%s -->", strings.ReplaceAll(run.ID, "-", ""))) {
			reports++
		}
	}
	if reports != 1 {
		t.Fatalf("delivered report comments = %d, want one", reports)
	}
}

// The retry intent and the requeue it asked for commit together, so an
// interruption cannot leave the request recorded without the attempt.
func TestRetryRequeueCommitsWithTheIntent(t *testing.T) {
	t.Parallel()

	_, runtime, api, _, cfg, r := testkit.TmuxFlow(t, "exit 1")
	publisher := &testkit.ReportGitHub{BranchGitHub: testkit.BranchGitHub{FakeGitHub: api}}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: publisher, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
	testkit.SeedStoppedReport(t, runtime, run.ID, 101)
	// A reset that cannot run rolls the whole request back.
	if _, err := runtime.DB.Exec(`CREATE TRIGGER fail_requeue BEFORE UPDATE ON report_publications BEGIN SELECT RAISE(ABORT, 'requeue failed'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Retry(context.Background(), run.ID); err == nil {
		t.Fatal("the retry accepted a failed requeue")
	}
	var pending int
	if err := runtime.DB.QueryRow(`SELECT count(*) FROM run_retries WHERE run_id=? AND pending=1`, run.ID).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("the retry intent survived a failed requeue: %d %v", pending, err)
	}
	saved, err := runtime.Workflow.Get(context.Background(), run.ID)
	if err != nil || len(saved.Retries) != 0 {
		t.Fatalf("the run kept a retry request: %+v %v", saved, err)
	}
	var stopped bool
	if err := runtime.DB.QueryRow(`SELECT stopped FROM report_publications WHERE run_id=?`, run.ID).Scan(&stopped); err != nil || !stopped {
		t.Fatalf("the stopped publication changed: %v %v", stopped, err)
	}
}
