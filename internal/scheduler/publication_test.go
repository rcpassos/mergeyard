package scheduler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestPublicationWarningDoesNotBlockCIReadiness(t *testing.T) {
	t.Parallel()

	flow := testkit.NewScenario(t)
	_, runtime, api, _, cfg, r := flow.Parts()
	publisher := &testkit.ReportGitHub{BranchGitHub: testkit.BranchGitHub{FakeGitHub: api}, Failure: true}
	checks := &testkit.CiGitHub{FakeGitHub: api, ApplyReady: true, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "success"}}}}
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	s, err := flow.New(cfg, scheduler.Dependencies{GitHub: testkit.PublishingCIGitHub{ReportGitHub: publisher, Checks: checks}, Runner: r, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	run := flow.Until(s, workflow.WaitingForCI, workflow.Review)
	if len(run.Publications) != 1 || run.Publications[0].WarningCode != "github.unavailable" || run.CI == nil {
		t.Fatal("publication or CI state lost")
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, err := runtime.Workflow.Get(context.Background(), run.ID)
	if err != nil || saved.State != workflow.ReadyToMerge || saved.LastErrorCode != "" || checks.ReadyCalls != 1 || len(saved.Publications) != 1 || saved.Publications[0].WarningCode != "github.unavailable" || !saved.CI.Deadline.Equal(run.CI.Deadline) {
		t.Fatalf("publication blocked readiness: %+v %v", saved, err)
	}
	server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil))
	for _, want := range []string{"Waiting for your merge", "CI and readiness", "Wait deadline", "Publication warning", "github.unavailable"} {
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), want) {
			t.Fatalf("missing %q from combined detail", want)
		}
	}
	response = httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/static/app.js", nil))
	for _, event := range []string{"publication.pending", "publication.warning", "publication.published", "publication.stopped", "ci.updated", "pr.readiness_started"} {
		if !strings.Contains(response.Body.String(), event) {
			t.Fatalf("missing live update %q", event)
		}
	}
}

func TestReportPublicationBacksOffTransientFailure(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	publisher := &testkit.ReportGitHub{Failure: true}
	s, rt, _ := testkit.CommentFlow(t, publisher, func() time.Time { return now })
	testkit.SeedReportPublication(t, rt, "report-transient", "MANUAL", 101)
	for range 2 {
		if _, err := s.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if publisher.Calls != 1 {
		t.Fatalf("report retried before its backoff: %d calls", publisher.Calls)
	}
	now = now.Add(time.Minute + time.Second)
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if publisher.Calls != 2 {
		t.Fatalf("report did not retry after backoff: %d calls", publisher.Calls)
	}
	var attempts int
	var stopped, failed bool
	if err := rt.DB.QueryRow(`SELECT attempts,failed,stopped FROM report_publications WHERE run_id='report-transient'`).Scan(&attempts, &failed, &stopped); err != nil || attempts != 2 || !failed || stopped {
		t.Fatalf("report retry state = %d %v %v, %v", attempts, failed, stopped, err)
	}
	if warnings := testkit.CountEvents(t, rt, `SELECT count(*) FROM events WHERE run_id='report-transient' AND type='publication.warning'`); warnings != 1 {
		t.Fatalf("report warnings = %d, want one", warnings)
	}
}

func TestReportPublicationStopsOnPermanentFailure(t *testing.T) {
	t.Parallel()

	publisher := &testkit.ReportGitHub{Failure: true, Code: "github.invalid_input"}
	s, rt, _ := testkit.CommentFlow(t, publisher, time.Now)
	testkit.SeedReportPublication(t, rt, "report-permanent", "MANUAL", 101)
	for range 3 {
		if _, err := s.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if publisher.Calls != 1 {
		t.Fatalf("permanent report failure retried: %d calls", publisher.Calls)
	}
	var stopped bool
	var code, warning string
	if err := rt.DB.QueryRow(`SELECT stopped,warning_code,warning FROM report_publications WHERE run_id='report-permanent'`).Scan(&stopped, &code, &warning); err != nil || !stopped || code != "github.invalid_input" || !strings.Contains(warning, "cannot deliver") {
		t.Fatalf("permanent report state = %v %q %q, %v", stopped, code, warning, err)
	}
}

func TestFinishedRunStopsRetryingReportPublication(t *testing.T) {
	t.Parallel()

	publisher := &testkit.ReportGitHub{Failure: true}
	s, rt, _ := testkit.CommentFlow(t, publisher, time.Now)
	testkit.SeedReportPublication(t, rt, "report-finished", "STOPPED", 101)
	for range 3 {
		if _, err := s.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if publisher.Calls != 1 {
		t.Fatalf("finished report retried: %d calls", publisher.Calls)
	}
	var stopped bool
	if err := rt.DB.QueryRow(`SELECT stopped FROM report_publications WHERE run_id='report-finished'`).Scan(&stopped); err != nil || !stopped {
		t.Fatalf("finished report was not stopped: %v, %v", stopped, err)
	}
	if warnings := testkit.CountEvents(t, rt, `SELECT count(*) FROM events WHERE run_id='report-finished' AND type='publication.warning'`); warnings != 1 {
		t.Fatalf("finished report warnings = %d, want one", warnings)
	}
}

func TestAttentionPublicationBacksOffTransientFailure(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	publisher := &testkit.ReportGitHub{Failure: true}
	s, rt, _ := testkit.CommentFlow(t, publisher, func() time.Time { return now })
	testkit.SeedAttentionPublication(t, rt, "attention-transient", "MANUAL", 101)
	for range 2 {
		if _, err := s.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if publisher.Calls != 1 {
		t.Fatalf("attention retried before its backoff: %d calls", publisher.Calls)
	}
	now = now.Add(time.Minute + time.Second)
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if publisher.Calls != 2 {
		t.Fatalf("attention did not retry after backoff: %d calls", publisher.Calls)
	}
	var attempts int
	var stopped, failed bool
	if err := rt.DB.QueryRow(`SELECT attempts,failed,stopped FROM attention_publications WHERE run_id='attention-transient'`).Scan(&attempts, &failed, &stopped); err != nil || attempts != 2 || !failed || stopped {
		t.Fatalf("attention retry state = %d %v %v, %v", attempts, failed, stopped, err)
	}
}

func TestAttentionPublicationStopsOnPermanentFailure(t *testing.T) {
	t.Parallel()

	publisher := &testkit.ReportGitHub{Failure: true, Code: "github.invalid_input"}
	s, rt, _ := testkit.CommentFlow(t, publisher, time.Now)
	testkit.SeedAttentionPublication(t, rt, "attention-permanent", "MANUAL", 101)
	for range 3 {
		if _, err := s.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if publisher.Calls != 1 {
		t.Fatalf("permanent attention failure retried: %d calls", publisher.Calls)
	}
	var stopped bool
	if err := rt.DB.QueryRow(`SELECT stopped FROM attention_publications WHERE run_id='attention-permanent'`).Scan(&stopped); err != nil || !stopped {
		t.Fatalf("permanent attention was not stopped: %v, %v", stopped, err)
	}
	if warnings := testkit.CountEvents(t, rt, `SELECT count(*) FROM events WHERE run_id='attention-permanent' AND type='publication.warning'`); warnings != 1 {
		t.Fatalf("permanent attention warnings = %d, want one", warnings)
	}
}

func TestFinishedRunStopsRetryingAttentionPublication(t *testing.T) {
	t.Parallel()

	publisher := &testkit.ReportGitHub{Failure: true}
	s, rt, _ := testkit.CommentFlow(t, publisher, time.Now)
	testkit.SeedAttentionPublication(t, rt, "attention-finished", "STOPPED", 101)
	for range 3 {
		if _, err := s.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if publisher.Calls != 1 {
		t.Fatalf("finished attention retried: %d calls", publisher.Calls)
	}
	var stopped bool
	if err := rt.DB.QueryRow(`SELECT stopped FROM attention_publications WHERE run_id='attention-finished'`).Scan(&stopped); err != nil || !stopped {
		t.Fatalf("finished attention was not stopped: %v, %v", stopped, err)
	}
	if warnings := testkit.CountEvents(t, rt, `SELECT count(*) FROM events WHERE run_id='attention-finished' AND type='publication.warning'`); warnings != 1 {
		t.Fatalf("finished attention warnings = %d, want one", warnings)
	}
}

func TestRunFinishingAfterTransientFailureStopsReportPublication(t *testing.T) {
	t.Parallel()

	publisher := &testkit.ReportGitHub{Failure: true}
	s, rt, _ := testkit.CommentFlow(t, publisher, time.Now)
	testkit.SeedReportPublication(t, rt, "report-finishes-later", "MANUAL", 101)
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if publisher.Calls != 1 {
		t.Fatalf("report attempts = %d, want 1", publisher.Calls)
	}
	if _, err := rt.DB.Exec(`UPDATE runs SET state='STOPPED' WHERE id='report-finishes-later'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if publisher.Calls != 1 {
		t.Fatalf("finished report retried: %d calls", publisher.Calls)
	}
	var stopped bool
	var warning string
	if err := rt.DB.QueryRow(`SELECT stopped,warning FROM report_publications WHERE run_id='report-finishes-later'`).Scan(&stopped, &warning); err != nil || !stopped || !strings.Contains(warning, "will not retry") || !strings.Contains(warning, "No further automatic retries") {
		t.Fatalf("finished report state = %v %q, %v", stopped, warning, err)
	}
	if err := rt.DB.QueryRow(`SELECT json_extract(payload_json,'$.message') FROM events WHERE type='publication.warning' AND json_extract(payload_json,'$.report_id')=(SELECT attempt_id FROM report_publications WHERE run_id='report-finishes-later') ORDER BY id DESC LIMIT 1`).Scan(&warning); err != nil || !strings.Contains(warning, "deferred") {
		t.Fatalf("report warning was rewritten: %q, %v", warning, err)
	}
	if err := rt.DB.QueryRow(`SELECT json_extract(payload_json,'$.message') FROM events WHERE type='publication.stopped' AND json_extract(payload_json,'$.report_id')=(SELECT attempt_id FROM report_publications WHERE run_id='report-finishes-later') ORDER BY id DESC LIMIT 1`).Scan(&warning); err != nil || !strings.Contains(warning, "will not retry") || !strings.Contains(warning, "No further automatic retries") {
		t.Fatalf("finished report stop event = %q, %v", warning, err)
	}
}

func TestRunFinishingAfterTransientFailureStopsAttentionPublication(t *testing.T) {
	t.Parallel()

	publisher := &testkit.ReportGitHub{Failure: true}
	s, rt, _ := testkit.CommentFlow(t, publisher, time.Now)
	testkit.SeedAttentionPublication(t, rt, "attention-finishes-later", "MANUAL", 101)
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if publisher.Calls != 1 {
		t.Fatalf("attention attempts = %d, want 1", publisher.Calls)
	}
	if _, err := rt.DB.Exec(`UPDATE runs SET state='STOPPED' WHERE id='attention-finishes-later'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if publisher.Calls != 1 {
		t.Fatalf("finished attention retried: %d calls", publisher.Calls)
	}
	var stopped bool
	if err := rt.DB.QueryRow(`SELECT stopped FROM attention_publications WHERE run_id='attention-finishes-later'`).Scan(&stopped); err != nil || !stopped {
		t.Fatalf("finished attention was not stopped: %v, %v", stopped, err)
	}
	var message string
	if err := rt.DB.QueryRow(`SELECT json_extract(payload_json,'$.message') FROM events WHERE type='publication.warning' AND json_extract(payload_json,'$.attention_event_id')=(SELECT event_id FROM attention_publications WHERE run_id='attention-finishes-later') ORDER BY id DESC LIMIT 1`).Scan(&message); err != nil || !strings.Contains(message, "deferred") {
		t.Fatalf("attention warning was rewritten: %q, %v", message, err)
	}
	if err := rt.DB.QueryRow(`SELECT json_extract(payload_json,'$.message') FROM events WHERE type='publication.stopped' AND json_extract(payload_json,'$.attention_event_id')=(SELECT event_id FROM attention_publications WHERE run_id='attention-finishes-later') ORDER BY id DESC LIMIT 1`).Scan(&message); err != nil || !strings.Contains(message, "will not retry") || !strings.Contains(message, "No further automatic retries") {
		t.Fatalf("attention stop event = %q, %v", message, err)
	}
}

func TestThrottledReportCommentBacksOffAndIsDelivered(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	runner := &testkit.GhComments{Refusals: []testkit.GhRefusal{{
		Stderr: "gh: Validation failed, or the endpoint has been spammed (HTTP 422)",
		Stdout: `{"message": "Validation failed, or the endpoint has been spammed"}`,
	}}}
	s, rt := testkit.GhCommentFlow(t, runner, func() time.Time { return now })
	testkit.SeedReportPublication(t, rt, "report-throttled", "MANUAL", 101)
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	var state, code string
	var stopped bool
	if err := rt.DB.QueryRow(`SELECT state,warning_code,stopped FROM report_publications WHERE run_id='report-throttled'`).Scan(&state, &code, &stopped); err != nil {
		t.Fatal(err)
	}
	if state != "pending" || code != "github.rate_limited" || stopped {
		t.Fatalf("throttled publication = %q %q %v", state, code, stopped)
	}
	if posts, _ := runner.Published(); posts != 1 {
		t.Fatalf("throttled publication attempts = %d, want 1", posts)
	}
	now = now.Add(time.Minute + time.Second)
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := rt.DB.QueryRow(`SELECT state,warning_code,stopped FROM report_publications WHERE run_id='report-throttled'`).Scan(&state, &code, &stopped); err != nil {
		t.Fatal(err)
	}
	if state != "published" || stopped || code != "" {
		t.Fatalf("delivered publication = %q %q %v", state, code, stopped)
	}
	posts, bodies := runner.Published()
	if posts != 2 || len(bodies) != 1 {
		t.Fatalf("published comments = %d attempts, %d bodies", posts, len(bodies))
	}
}

func TestPermanentRefusalStopsPublicationWithWarning(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		refusal testkit.GhRefusal
	}{
		{"oversized body", testkit.GhRefusal{
			Stderr: "gh: Body is too long (maximum is 65536 characters) (HTTP 422)",
			Stdout: `{"message": "Body is too long (maximum is 65536 characters)"}`,
		}},
		{"invalid field", testkit.GhRefusal{
			Stderr: "gh: Validation Failed (HTTP 422)",
			Stdout: `{"message": "Validation Failed", "errors": [{"resource": "IssueComment", "field": "body", "code": "missing"}]}`,
		}},
		{"nested size error", testkit.GhRefusal{
			Stderr: "gh: Validation Failed (HTTP 422)",
			Stdout: `{"message": "Validation Failed", "errors": [{"resource": "IssueComment", "field": "body", "code": "custom", "message": "body is too long (maximum is 65536 characters)"}]}`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &testkit.GhComments{Refusals: []testkit.GhRefusal{tc.refusal}}
			s, rt := testkit.GhCommentFlow(t, runner, time.Now)
			testkit.SeedReportPublication(t, rt, "report-permanent", "MANUAL", 101)
			for range 3 {
				if _, err := s.Reconcile(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			var stopped bool
			var code, warning string
			if err := rt.DB.QueryRow(`SELECT stopped,warning_code,warning FROM report_publications WHERE run_id='report-permanent'`).Scan(&stopped, &code, &warning); err != nil {
				t.Fatal(err)
			}
			if !stopped || code != "github.invalid_input" || !strings.Contains(warning, "cannot deliver") {
				t.Fatalf("permanent publication = %v %q %q", stopped, code, warning)
			}
			if posts, _ := runner.Published(); posts != 1 {
				t.Fatalf("permanent publication retried: %d", posts)
			}
		})
	}
}
