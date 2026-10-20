package scheduler_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// A forbidden answer is permanent: the publication stops for good until a
// human says the cause is fixed.
func TestRequeueDeliversStoppedReportPublicationAfterTheCauseIsFixed(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	publisher := &reportGitHub{failure: true, code: "github.forbidden"}
	s, rt, cfg := commentFlow(t, publisher, func() time.Time { return now })
	ctx := context.Background()
	seedReportPublication(t, rt, "report-requeue", "MANUAL", 101)
	for range 3 {
		if _, err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if publisher.calls != 1 {
		t.Fatalf("permanent report failure retried: %d calls", publisher.calls)
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
	server.ServeHTTP(response, dashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/report-requeue", nil))
	for _, want := range []string{"Stopped publications", "github.forbidden", "cannot deliver", "Requeue publications"} {
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), want) {
			t.Fatalf("run detail missing %q: %d %s", want, response.Code, response.Body.String())
		}
	}
	// The human fixed the token and asks for another attempt.
	publisher.failure = false
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
	if len(publisher.bodies) != 1 {
		t.Fatalf("delivered comments = %d, want one", len(publisher.bodies))
	}
	// The requeued publication is no longer a stopped one.
	remaining, err := s.StoppedPublications(ctx, "report-requeue")
	if err != nil || len(remaining) != 0 {
		t.Fatalf("stopped publications after delivery = %+v, %v", remaining, err)
	}
}

func TestRequeueDeliversStoppedAttentionPublicationAfterTheCauseIsFixed(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	publisher := &reportGitHub{failure: true, code: "github.forbidden"}
	s, rt, _ := commentFlow(t, publisher, func() time.Time { return now })
	ctx := context.Background()
	seedAttentionPublication(t, rt, "attention-requeue", "MANUAL", 101)
	for range 3 {
		if _, err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if publisher.calls != 1 {
		t.Fatalf("permanent attention failure retried: %d calls", publisher.calls)
	}
	var stopped bool
	var code, warning string
	if err := rt.DB.QueryRow(`SELECT stopped,warning_code,warning FROM attention_publications WHERE run_id='attention-requeue'`).Scan(&stopped, &code, &warning); err != nil || !stopped || code != "github.forbidden" || !strings.Contains(warning, "cannot deliver") {
		t.Fatalf("stopped attention = %v %q %q, %v", stopped, code, warning, err)
	}
	publisher.failure = false
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
	if len(publisher.bodies) != 1 {
		t.Fatalf("delivered comments = %d, want one", len(publisher.bodies))
	}
}

func TestRequeueDeliversStoppedVerificationBodyAfterTheCauseIsFixed(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	s, rt, publisher, _ := failingVerificationFlowAt(t, "github.forbidden", func() time.Time { return now })
	ctx := context.Background()
	seedVerificationRun(t, rt, "probe-requeue", "MANUAL", 7, 101)
	for range 3 {
		if _, err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if publisher.updates != 1 {
		t.Fatalf("permanent body failure retried: %d updates", publisher.updates)
	}
	var stopped bool
	var code, warning string
	if err := rt.DB.QueryRow(`SELECT stopped,warning_code,warning FROM verification_attempts WHERE run_id='probe-requeue'`).Scan(&stopped, &code, &warning); err != nil || !stopped || code != "github.forbidden" {
		t.Fatalf("stopped body = %v %q %q, %v", stopped, code, warning, err)
	}
	publisher.offline = false
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
	if publisher.updates != 2 {
		t.Fatalf("requeued body updates = %d, want two", publisher.updates)
	}
	var attempts int
	if err := rt.DB.QueryRow(`SELECT stopped,warning_code,attempts FROM verification_attempts WHERE run_id='probe-requeue'`).Scan(&stopped, &code, &attempts); err != nil || stopped || code != "" || attempts != 1 {
		t.Fatalf("requeued body = %v %q %d, %v", stopped, code, attempts, err)
	}
}

// ghMarkedComments answers the gh calls of one publication whose comment is
// already on the pull request, so a requeue must not create a second one.
type ghMarkedComments struct {
	mu       sync.Mutex
	existing string
	posts    int
}

func (g *ghMarkedComments) Run(_ context.Context, _ []byte, args ...string) ([]byte, []byte, error) {
	method, endpoint := ghCall(args)
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case method == "GET" && strings.HasSuffix(endpoint, "/comments"):
		if g.existing == "" {
			return []byte("[]"), nil, nil
		}
		return []byte(`[{"id":9,"html_url":"https://github.com/owner/repo/pull/101#issuecomment-9","body":` + strconv.Quote(g.existing) + `}]`), nil, nil
	case method == "POST" && strings.HasSuffix(endpoint, "/comments"):
		g.posts++
		return nil, nil, errors.New("the comment was already delivered")
	}
	return nil, nil, fmt.Errorf("unexpected gh call: %v", args)
}

func TestRequeueFindsAnAlreadyDeliveredComment(t *testing.T) {
	runner := &ghMarkedComments{}
	s, rt := ghCommentFlow(t, runner, time.Now)
	ctx := context.Background()
	seedReportPublication(t, rt, "report-marker", "MANUAL", 101)
	// The comment reached the pull request before the failure that stopped the
	// publication, so the marker lookup must find it again.
	runner.existing = "<!-- mergeyard:report:reportmarker -->\nReport"
	if _, err := rt.DB.ExecContext(ctx, `UPDATE report_publications SET stopped=1,failed=1,warning_code='github.forbidden',warning='Report is saved locally. PR comment publication cannot deliver this comment.' WHERE run_id='report-marker'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequeuePublications(ctx, "report-marker"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if runner.posts != 0 {
		t.Fatalf("requeue created %d comments, want none", runner.posts)
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

// seedStoppedReport inserts a stopped report publication for a run that
// already exists.
func seedStoppedReport(t *testing.T, rt *app.Runtime, runID string, pr int) {
	t.Helper()
	ctx := context.Background()
	if _, err := rt.DB.ExecContext(ctx, `INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status,step) VALUES(?,?,'review','reviewer',1,1,'claude','succeeded','')`, runID+"-attempt", runID); err != nil {
		t.Fatal(err)
	}
	marker := fmt.Sprintf("<!-- mergeyard:report:%s -->", strings.ReplaceAll(runID, "-", ""))
	if _, err := rt.DB.ExecContext(ctx, `INSERT INTO report_publications(attempt_id,run_id,repository,pr_number,phase,round,attempt,body,state,stopped,failed,warning_code,warning)
		VALUES(?,?,'owner/repo',?,'review',1,1,?,'pending',1,1,'github.forbidden','Report is saved locally. PR comment publication cannot deliver this comment.')`,
		runID+"-attempt", runID, pr, marker+"\nReport"); err != nil {
		t.Fatal(err)
	}
}

// Retry asks for another attempt, so the publications that stopped for good
// get the same chance.
func TestRetryRequeuesStoppedPublications(t *testing.T) {
	_, runtime, api, _, cfg, r := localFlow(t, "exit 1")
	publisher := &reportGitHub{branchGitHub: branchGitHub{api}}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: publisher, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.NeedsAttention, workflow.Implement)
	seedStoppedReport(t, runtime, run.ID, 101)
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
	for _, body := range publisher.bodies {
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
	_, runtime, api, _, cfg, r := localFlow(t, "exit 1")
	publisher := &reportGitHub{branchGitHub: branchGitHub{api}}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: publisher, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.NeedsAttention, workflow.Implement)
	seedStoppedReport(t, runtime, run.ID, 101)
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

func TestRequeueReportsAnUnknownRun(t *testing.T) {
	publisher := &reportGitHub{}
	s, _, _ := commentFlow(t, publisher, time.Now)
	if _, err := s.RequeuePublications(context.Background(), "missing-run"); err == nil {
		t.Fatal("requeue accepted an unknown run")
	}
}

// The dashboard action requeues the publications of the run it shows.
func TestDashboardRequeuesStoppedPublications(t *testing.T) {
	publisher := &reportGitHub{failure: true, code: "github.forbidden"}
	s, rt, cfg := commentFlow(t, publisher, time.Now)
	ctx := context.Background()
	seedReportPublication(t, rt, "report-dashboard", "MANUAL", 101)
	for range 3 {
		if _, err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	request := dashboardRequest(t, server, http.MethodPost, "http://127.0.0.1:7331/runs/report-dashboard/requeue", strings.NewReader(url.Values{"csrf_token": {csrfToken(t, rt, server)}}.Encode()))
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
	server.ServeHTTP(page, dashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/report-dashboard", nil))
	if strings.Contains(page.Body.String(), "Stopped publications") {
		t.Fatalf("requeued publication is still listed as stopped: %s", page.Body.String())
	}
}

// The CLI API requeues the same publications the dashboard does.
func TestRequeueAPIAndDashboardShareTheScheduler(t *testing.T) {
	publisher := &reportGitHub{failure: true, code: "github.forbidden"}
	s, rt, cfg := commentFlow(t, publisher, time.Now)
	ctx := context.Background()
	seedReportPublication(t, rt, "report-api", "MANUAL", 101)
	for range 3 {
		if _, err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	token := csrfToken(t, rt, server)
	// An unprotected request changes nothing.
	for _, invalid := range []string{"origin", "token"} {
		request := dashboardRequest(t, server, http.MethodPost, "http://127.0.0.1:7331/api/runs/report-api/requeue", nil)
		request.Header.Set("Origin", "http://127.0.0.1:7331")
		request.Header.Set("X-CSRF-Token", token)
		signAPIRequest(t, rt.Workspace.Root, request)
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
	request := dashboardRequest(t, server, http.MethodPost, "http://127.0.0.1:7331/api/runs/report-api/requeue", nil)
	request.Header.Set("Origin", "http://127.0.0.1:7331")
	request.Header.Set("X-CSRF-Token", token)
	signAPIRequest(t, rt.Workspace.Root, request)
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

// csrfToken reads the token the CLI API hands to a proven caller.
func csrfToken(t *testing.T, rt *app.Runtime, server *web.Server) string {
	t.Helper()
	response := httptest.NewRecorder()
	server.ServeHTTP(response, apiRequest(t, rt.Workspace.Root, http.MethodGet, "http://127.0.0.1:7331/api/status"))
	var status web.Status
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	return status.Token
}
