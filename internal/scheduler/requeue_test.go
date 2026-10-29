package scheduler_test

import (
	"context"
	"encoding/json"

	"net/http"
	"net/http/httptest"
	"net/url"

	"strings"

	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	// A forbidden answer is permanent: the publication stops for good until a
	// human says the cause is fixed.
)

func TestRequeueDeliversStoppedReportPublicationAfterTheCauseIsFixed(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	publisher := &testkit.ReportGitHub{Failure: true, Code: "github.forbidden"}
	s, rt, cfg := testkit.CommentFlow(t, publisher, func() time.Time { return now })
	ctx := context.Background()
	testkit.SeedReportPublication(t, rt, "report-requeue", "MANUAL", 101)
	for range 3 {
		if _, err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if publisher.Calls != 1 {
		t.Fatalf("permanent report failure retried: %d calls", publisher.Calls)
	}
	var stopped bool
	var code, warning string
	if err := rt.DB.QueryRow(`SELECT stopped,warning_code,warning FROM report_publications WHERE run_id='report-requeue'`).Scan(&stopped, &code, &warning); err != nil || !stopped || code != "github.forbidden" {
		t.Fatalf("stopped report = %v %q %q, %v", stopped, code, warning, err)
	}
	// The reason stays visible in the run detail until the human requeues it.
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/report-requeue", nil))
	for _, want := range []string{"Stopped publications", "github.forbidden", "cannot deliver", "Requeue publications"} {
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), want) {
			t.Fatalf("run detail missing %q: %d %s", want, response.Code, response.Body.String())
		}
	}
	// The human fixed the token and asks for another attempt.
	publisher.Failure = false
	requeued, err := s.RequeuePublications(ctx, "report-requeue")
	if err != nil {
		t.Fatal(err)
	}
	if len(requeued) != 1 || requeued[0].Kind != "report" || requeued[0].Code != "github.forbidden" {
		t.Fatalf("requeued publications = %+v", requeued)
	}
	// The requeue starts a fresh backoff, so the comment is delivered at once.
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	var attempts int
	if err := rt.DB.QueryRow(`SELECT state,attempts,warning_code FROM report_publications WHERE run_id='report-requeue'`).Scan(&state, &attempts, &code); err != nil {
		t.Fatal(err)
	}
	if state != "published" || code != "" || attempts != 1 {
		t.Fatalf("requeued publication = %q %d %q", state, attempts, code)
	}
	if len(publisher.Bodies) != 1 {
		t.Fatalf("delivered comments = %d, want one", len(publisher.Bodies))
	}
	// The requeued publication is no longer a stopped one.
	remaining, err := s.StoppedPublications(ctx, "report-requeue")
	if err != nil || len(remaining) != 0 {
		t.Fatalf("stopped publications after delivery = %+v, %v", remaining, err)
	}
}

func TestRequeueDeliversStoppedAttentionPublicationAfterTheCauseIsFixed(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	publisher := &testkit.ReportGitHub{Failure: true, Code: "github.forbidden"}
	s, rt, _ := testkit.CommentFlow(t, publisher, func() time.Time { return now })
	ctx := context.Background()
	testkit.SeedAttentionPublication(t, rt, "attention-requeue", "MANUAL", 101)
	for range 3 {
		if _, err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if publisher.Calls != 1 {
		t.Fatalf("permanent attention failure retried: %d calls", publisher.Calls)
	}
	var stopped bool
	var code, warning string
	if err := rt.DB.QueryRow(`SELECT stopped,warning_code,warning FROM attention_publications WHERE run_id='attention-requeue'`).Scan(&stopped, &code, &warning); err != nil || !stopped || code != "github.forbidden" || !strings.Contains(warning, "cannot deliver") {
		t.Fatalf("stopped attention = %v %q %q, %v", stopped, code, warning, err)
	}
	publisher.Failure = false
	requeued, err := s.RequeuePublications(ctx, "attention-requeue")
	if err != nil {
		t.Fatal(err)
	}
	if len(requeued) != 1 || requeued[0].Kind != "attention" {
		t.Fatalf("requeued publications = %+v", requeued)
	}
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := rt.DB.QueryRow(`SELECT state,warning_code FROM attention_publications WHERE run_id='attention-requeue'`).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "published" || code != "" {
		t.Fatalf("requeued attention = %q %q", state, code)
	}
	if len(publisher.Bodies) != 1 {
		t.Fatalf("delivered comments = %d, want one", len(publisher.Bodies))
	}
}

func TestRequeueDeliversStoppedVerificationBodyAfterTheCauseIsFixed(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	s, rt, publisher, _ := testkit.FailingVerificationFlowAt(t, "github.forbidden", func() time.Time { return now })
	ctx := context.Background()
	testkit.SeedVerificationRun(t, rt, "probe-requeue", "MANUAL", 7, 101)
	for range 3 {
		if _, err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if publisher.Updates != 1 {
		t.Fatalf("permanent body failure retried: %d updates", publisher.Updates)
	}
	var stopped bool
	var code, warning string
	if err := rt.DB.QueryRow(`SELECT stopped,warning_code,warning FROM verification_attempts WHERE run_id='probe-requeue'`).Scan(&stopped, &code, &warning); err != nil || !stopped || code != "github.forbidden" {
		t.Fatalf("stopped body = %v %q %q, %v", stopped, code, warning, err)
	}
	publisher.Offline = false
	requeued, err := s.RequeuePublications(ctx, "probe-requeue")
	if err != nil {
		t.Fatal(err)
	}
	if len(requeued) != 1 || requeued[0].Kind != "verification" {
		t.Fatalf("requeued publications = %+v", requeued)
	}
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if publisher.Updates != 2 {
		t.Fatalf("requeued body updates = %d, want two", publisher.Updates)
	}
	var attempts int
	if err := rt.DB.QueryRow(`SELECT stopped,warning_code,attempts FROM verification_attempts WHERE run_id='probe-requeue'`).Scan(&stopped, &code, &attempts); err != nil || stopped || code != "" || attempts != 1 {
		t.Fatalf("requeued body = %v %q %d, %v", stopped, code, attempts, err)
	}
}

func TestRequeueFindsAnAlreadyDeliveredComment(t *testing.T) {
	t.Parallel()

	runner := &testkit.GhMarkedComments{}
	s, rt := testkit.GhCommentFlow(t, runner, time.Now)
	ctx := context.Background()
	testkit.SeedReportPublication(t, rt, "report-marker", "MANUAL", 101)
	// The comment reached the pull request before the failure that stopped the
	// publication, so the marker lookup must find it again.
	runner.Existing = "<!-- mergeyard:report:reportmarker -->\nReport"
	if _, err := rt.DB.ExecContext(ctx, `UPDATE report_publications SET stopped=1,failed=1,warning_code='github.forbidden',warning='Report is saved locally. PR comment publication cannot deliver this comment.' WHERE run_id='report-marker'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequeuePublications(ctx, "report-marker"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if runner.Posts != 0 {
		t.Fatalf("requeue created %d comments, want none", runner.Posts)
	}
	var state string
	var commentID int64
	if err := rt.DB.QueryRow(`SELECT state,comment_id FROM report_publications WHERE run_id='report-marker'`).Scan(&state, &commentID); err != nil {
		t.Fatal(err)
	}
	if state != "published" || commentID != 9 {
		t.Fatalf("requeued publication = %q %d", state, commentID)
	}
}

func TestRequeueReportsAnUnknownRun(t *testing.T) {
	t.Parallel()

	publisher := &testkit.ReportGitHub{}
	s, _, _ := testkit.CommentFlow(t, publisher, time.Now)
	if _, err := s.RequeuePublications(context.Background(), "missing-run"); err == nil {
		t.Fatal("requeue accepted an unknown run")
	}
}

// The dashboard action requeues the publications of the run it shows.
func TestDashboardRequeuesStoppedPublications(t *testing.T) {
	t.Parallel()

	publisher := &testkit.ReportGitHub{Failure: true, Code: "github.forbidden"}
	s, rt, cfg := testkit.CommentFlow(t, publisher, time.Now)
	ctx := context.Background()
	testkit.SeedReportPublication(t, rt, "report-dashboard", "MANUAL", 101)
	for range 3 {
		if _, err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	request := testkit.DashboardRequest(t, server, http.MethodPost, "http://127.0.0.1:7331/runs/report-dashboard/requeue", strings.NewReader(url.Values{"csrf_token": {testkit.CsrfToken(t, rt, server)}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://127.0.0.1:7331")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("requeue page: %d %s", response.Code, response.Body.String())
	}
	var stopped bool
	if err := rt.DB.QueryRow(`SELECT stopped FROM report_publications WHERE run_id='report-dashboard'`).Scan(&stopped); err != nil || stopped {
		t.Fatalf("dashboard requeue left the publication stopped: %v, %v", stopped, err)
	}
	page := httptest.NewRecorder()
	server.ServeHTTP(page, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/report-dashboard", nil))
	if strings.Contains(page.Body.String(), "Stopped publications") {
		t.Fatalf("requeued publication is still listed as stopped: %s", page.Body.String())
	}
}

// The CLI API requeues the same publications the dashboard does.
func TestRequeueAPIAndDashboardShareTheScheduler(t *testing.T) {
	t.Parallel()

	publisher := &testkit.ReportGitHub{Failure: true, Code: "github.forbidden"}
	s, rt, cfg := testkit.CommentFlow(t, publisher, time.Now)
	ctx := context.Background()
	testkit.SeedReportPublication(t, rt, "report-api", "MANUAL", 101)
	for range 3 {
		if _, err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	token := testkit.CsrfToken(t, rt, server)
	// An unprotected request changes nothing.
	for _, invalid := range []string{"origin", "token"} {
		request := testkit.DashboardRequest(t, server, http.MethodPost, "http://127.0.0.1:7331/api/runs/report-api/requeue", nil)
		request.Header.Set("Origin", "http://127.0.0.1:7331")
		request.Header.Set("X-CSRF-Token", token)
		testkit.SignAPIRequest(t, rt.Workspace.Root, request)
		switch invalid {
		case "origin":
			request.Header.Set("Origin", "https://evil.example")
		case "token":
			request.Header.Del("X-CSRF-Token")
		}
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("unprotected requeue: %d", response.Code)
		}
	}
	var stopped bool
	if err := rt.DB.QueryRow(`SELECT stopped FROM report_publications WHERE run_id='report-api'`).Scan(&stopped); err != nil || !stopped {
		t.Fatalf("unprotected requeue changed the publication: %v, %v", stopped, err)
	}
	request := testkit.DashboardRequest(t, server, http.MethodPost, "http://127.0.0.1:7331/api/runs/report-api/requeue", nil)
	request.Header.Set("Origin", "http://127.0.0.1:7331")
	request.Header.Set("X-CSRF-Token", token)
	testkit.SignAPIRequest(t, rt.Workspace.Root, request)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("requeue API: %d %s", response.Code, response.Body.String())
	}
	var result web.RequeueResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.RunID != "report-api" || result.Requeued != 1 || len(result.Publications) != 1 {
		t.Fatalf("requeue answer = %+v", result)
	}
	if err := rt.DB.QueryRow(`SELECT stopped FROM report_publications WHERE run_id='report-api'`).Scan(&stopped); err != nil || stopped {
		t.Fatalf("requeue left the publication stopped: %v, %v", stopped, err)
	}
}
