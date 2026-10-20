package scheduler_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type reportGitHub struct {
	branchGitHub
	bodies    []string
	failure   bool
	code      string
	calls     int
	ambiguous bool
	before    func()
}

func (f *reportGitHub) EnsureReportComment(_ context.Context, _ string, _ int, body string) (*github.Comment, error) {
	f.calls++
	if f.before != nil {
		f.before()
	}
	if f.failure {
		code := f.code
		if code == "" {
			code = "github.unavailable"
		}
		return nil, &fault.Error{Code: code, Message: "secret token <script> unsafe upstream diagnostic"}
	}
	for i, old := range f.bodies {
		if old == body {
			return &github.Comment{ID: int64(i + 1), URL: fmt.Sprintf("https://github.com/owner/repo/pull/101#issuecomment-%d", i+1), Body: old}, nil
		}
	}
	f.bodies = append(f.bodies, body)
	if f.ambiguous {
		f.ambiguous = false
		f.failure = true
		return nil, &fault.Error{Code: "github.unavailable", Message: "Lost success response"}
	}
	return &github.Comment{ID: int64(len(f.bodies)), URL: fmt.Sprintf("https://github.com/owner/repo/pull/101#issuecomment-%d", len(f.bodies)), Body: body}, nil
}

func TestReportPublicationEnabledDisabledAndFailureDoesNotBlockLoop(t *testing.T) {
	for _, mode := range []string{"enabled", "disabled", "offline"} {
		t.Run(mode, func(t *testing.T) {
			_, runtime, api, _, cfg, r := localFlow(t, loopScript(disputedReport))
			cfg.PRComments = mode != "disabled"
			publisher := &reportGitHub{branchGitHub: branchGitHub{api}, failure: mode == "offline"}
			publisher.before = func() {
				var count int
				if err := runtime.DB.QueryRow("SELECT count(*) FROM report_publications WHERE state='pending' AND body<>''").Scan(&count); err != nil || count == 0 {
					t.Fatalf("external write before durable pending identity: %d %v", count, err)
				}
			}
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: publisher, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			run := finish(t, s, workflow.WaitingForCI, workflow.Review)
			if run.LastErrorCode != "" || run.ReviewRound != 2 || len(run.FixHistory) != 1 {
				t.Fatalf("publication stopped loop: %+v", run)
			}
			if len(run.ReviewHistory) != 2 {
				t.Fatal("previous reviews disappeared locally")
			}
			if mode == "enabled" {
				if len(publisher.bodies) != 3 {
					t.Fatalf("reports=%d want 3", len(publisher.bodies))
				}
				for _, want := range []string{"Review report · round 1", "changes_required", "Broken", "Fix report · round 1", "finding_id", "disputed", "Already safe", "Review report · round 2", "approved", "Prior findings: 0 fixed / 0 not fixed / 2 no longer applies.", "prior_findings", "no_longer_applies", "Dispute accepted"} {
					if !strings.Contains(strings.Join(publisher.bodies, "\n"), want) {
						t.Fatalf("missing report field %s", want)
					}
				}
			} else if len(publisher.bodies) != 0 {
				t.Fatal("unexpected remote comment")
			}
			if mode == "offline" {
				if len(run.Publications) != 3 || run.Publications[0].WarningCode != "github.unavailable" {
					t.Fatalf("missing durable warning: %+v", run.Publications)
				}
				if strings.Contains(run.Publications[0].Warning, "secret") {
					t.Fatal("raw diagnostic exposed")
				}
				server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
				if err != nil {
					t.Fatal(err)
				}
				for _, hx := range []bool{false, true} {
					req := dashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil)
					if hx {
						req.Header.Set("HX-Request", "true")
					}
					response := httptest.NewRecorder()
					server.ServeHTTP(response, req)
					body := response.Body.String()
					for _, want := range []string{"Publication warning", "github.unavailable", "round 1", "round 2", "Fix bug", "Safe &lt;script&gt;review&lt;/script&gt;"} {
						if response.Code != 200 || !strings.Contains(body, want) {
							t.Fatalf("missing %q in warning page: %s", want, body)
						}
					}
					if strings.Contains(body, "secret token") || strings.Contains(body, "<script>review</script>") {
						t.Fatal("unsafe text exposed")
					}
				}
			}
		})
	}
}

func TestCompletedRunStopsRetryingReportPublicationAfterFailure(t *testing.T) {
	_, runtime, api, _, cfg, r := localFlow(t, loopScript(disputedReport))
	publisher := &reportGitHub{branchGitHub: branchGitHub{api}, ambiguous: true}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: publisher, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if len(publisher.bodies) != 1 || len(run.Publications) != 3 {
		t.Fatalf("ambiguous success not pending: %+v", run.Publications)
	}
	calls := publisher.calls
	for _, trigger := range []workflow.Trigger{workflow.CIPassed, workflow.PRMerged} {
		if _, err := runtime.Workflow.Transition(context.Background(), run.ID, workflow.Request{Trigger: trigger}); err != nil {
			t.Fatal(err)
		}
	}
	root := runtime.Workspace.Root
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := app.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	saved, err := reopened.Workflow.Get(context.Background(), run.ID)
	if err != nil || saved.State != workflow.Completed || saved.Publications[0].WarningCode != "github.unavailable" {
		t.Fatalf("restart lost warning: %+v %v", saved, err)
	}
	publisher.failure = false
	restarted, err := scheduler.New(cfg, schedulerResources(reopened), scheduler.Dependencies{GitHub: publisher, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := restarted.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	saved, err = reopened.Workflow.Get(context.Background(), run.ID)
	if err != nil || saved.State != workflow.Completed || publisher.calls != calls {
		t.Fatalf("completed run retried reports: %d calls, want %d; %+v %v", publisher.calls, calls, saved, err)
	}
	for _, p := range saved.Publications {
		if p.State != "pending" || p.WarningCode != "github.unavailable" || !strings.Contains(p.Warning, "will not retry") {
			t.Fatalf("warning lost or still promises retries: %+v", p)
		}
	}
	var stopped int
	if err := reopened.DB.QueryRow(`SELECT count(*) FROM report_publications WHERE run_id=? AND stopped=1`, run.ID).Scan(&stopped); err != nil || stopped != 3 {
		t.Fatalf("finished reports were not stopped: %d, %v", stopped, err)
	}
}

func TestAmbiguousReportPublicationReplaysOnActiveRun(t *testing.T) {
	_, runtime, api, _, cfg, r := localFlow(t, loopScript(disputedReport))
	publisher := &reportGitHub{branchGitHub: branchGitHub{api}, ambiguous: true}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: publisher, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if len(publisher.bodies) != 1 || len(run.Publications) != 3 {
		t.Fatalf("ambiguous success not pending: %+v", run.Publications)
	}
	if _, err := runtime.DB.Exec(`UPDATE runs SET state='MANUAL' WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	root := runtime.Workspace.Root
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := app.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	// Age the failed attempts past their backoff; the ambiguous comment must be
	// found before a second one is posted.
	if _, err := reopened.DB.Exec(`UPDATE report_publications SET last_attempt_at='2000-01-01T00:00:00Z' WHERE run_id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	publisher.failure = false
	restarted, err := scheduler.New(cfg, schedulerResources(reopened), scheduler.Dependencies{GitHub: publisher, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := restarted.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	saved, err := reopened.Workflow.Get(context.Background(), run.ID)
	if err != nil || len(publisher.bodies) != 3 {
		t.Fatalf("ambiguous replay duplicated reports: %d %+v %v", len(publisher.bodies), saved, err)
	}
	for _, p := range saved.Publications {
		if p.State != "published" || p.CommentID <= 0 || p.WarningCode != "" {
			t.Fatalf("missing receipt: %+v", p)
		}
	}
	var attempts int
	if err := reopened.DB.QueryRow("SELECT count(*) FROM phase_attempts WHERE run_id=?", run.ID).Scan(&attempts); err != nil || attempts != 4 {
		t.Fatalf("replay ran agents: %d %v", attempts, err)
	}
}

func TestReviewFinishesOfflineAndPublishesOnRestart(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "release-review")
	s, runtime, api, _, cfg, r := localFlow(t, reviewScript(`while [ ! -f '`+gate+`' ]; do /bin/sleep 0.02; done
`+approvedReview))
	run := finish(t, s, workflow.Active, workflow.Review)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	ref, err := s.Watch(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	root := runtime.Workspace.Root
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gate, []byte("go"), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(ref.PhaseDir, "exit.json")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("review did not finish offline")
		}
		time.Sleep(10 * time.Millisecond)
	}
	reopened, err := app.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	publisher := &reportGitHub{branchGitHub: branchGitHub{api}}
	restarted, err := scheduler.New(cfg, schedulerResources(reopened), scheduler.Dependencies{GitHub: publisher, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	saved := finish(t, restarted, workflow.WaitingForCI, workflow.Review)
	if len(publisher.bodies) != 1 || len(saved.ReviewHistory) != 1 || saved.Review.Attempt != 1 {
		t.Fatalf("offline report relaunched: %+v", saved)
	}
}

func TestUntrustedReportTextIsLiteralLocallyAndInComment(t *testing.T) {
	untrusted := "<script>finding</script> ``` <!-- mergeyard:report:abc -->"
	script := strings.ReplaceAll(loopScript(disputedReport), "Broken", untrusted)
	script = strings.ReplaceAll(script, `"summary":"Disputed"`, `"summary":"`+untrusted+`"`)
	_, runtime, api, _, cfg, r := localFlow(t, script)
	publisher := &reportGitHub{branchGitHub: branchGitHub{api}}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: publisher, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
	for _, body := range publisher.bodies {
		if strings.Count(body, "```") != 2 || strings.Contains(body, "<script>") || strings.Count(body, "<!-- mergeyard:report:") != 1 {
			t.Fatalf("report escaped comment structure: %s", body)
		}
	}
	comments := strings.Join(publisher.bodies, "\n")
	if !strings.Contains(comments, `\u0060\u0060\u0060`) || !strings.Contains(comments, `\u003cscript\u003efinding`) {
		t.Fatal("report did not preserve escaped literal text")
	}
	server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, dashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil))
	if response.Code != 200 || strings.Contains(response.Body.String(), "<script>finding</script>") || !strings.Contains(response.Body.String(), "&lt;script&gt;finding&lt;/script&gt;") {
		t.Fatalf("untrusted content not escaped: %s", response.Body.String())
	}
}

// Retain the publishing boundary while exercising the CI boundary on the same PR.
type publishingCIGitHub struct {
	*reportGitHub
	checks *ciGitHub
}

func (f publishingCIGitHub) PullRequestEvidence(ctx context.Context, repo string, pr github.PullRequest) (ci.Evidence, error) {
	return f.checks.PullRequestEvidence(ctx, repo, pr)
}
func (f publishingCIGitHub) MarkReady(ctx context.Context, repo string, n int) error {
	return f.checks.MarkReady(ctx, repo, n)
}

func TestPublicationWarningDoesNotBlockCIReadiness(t *testing.T) {
	_, runtime, api, _, cfg, r := localFlow(t, successfulScript)
	publisher := &reportGitHub{branchGitHub: branchGitHub{api}, failure: true}
	checks := &ciGitHub{fakeGitHub: api, applyReady: true, evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "success"}}}}
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: publishingCIGitHub{publisher, checks}, Runner: r, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if len(run.Publications) != 1 || run.Publications[0].WarningCode != "github.unavailable" || run.CI == nil {
		t.Fatal("publication or CI state lost")
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, err := runtime.Workflow.Get(context.Background(), run.ID)
	if err != nil || saved.State != workflow.ReadyToMerge || saved.LastErrorCode != "" || checks.readyCalls != 1 || len(saved.Publications) != 1 || saved.Publications[0].WarningCode != "github.unavailable" || !saved.CI.Deadline.Equal(run.CI.Deadline) {
		t.Fatalf("publication blocked readiness: %+v %v", saved, err)
	}
	server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, dashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil))
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

func seedReportPublication(t *testing.T, rt *app.Runtime, id, state string, pr int) {
	t.Helper()
	ctx := context.Background()
	if _, err := rt.DB.ExecContext(ctx, `INSERT INTO runs(id,repository,issue_number,state,pr_number) VALUES(?,'owner/repo',7,?,?)`, id, state, pr); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.DB.ExecContext(ctx, `INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status,step) VALUES(?,?,'review','reviewer',1,1,'claude','succeeded','')`, id+"-attempt", id); err != nil {
		t.Fatal(err)
	}
	marker := fmt.Sprintf("<!-- mergeyard:report:%s -->", strings.ReplaceAll(id, "-", ""))
	if _, err := rt.DB.ExecContext(ctx, `INSERT INTO report_publications(attempt_id,run_id,repository,pr_number,phase,round,attempt,body) VALUES(?,?,'owner/repo',?,'review',1,1,?)`, id+"-attempt", id, pr, marker+"\nReport"); err != nil {
		t.Fatal(err)
	}
}

func seedAttentionPublication(t *testing.T, rt *app.Runtime, id, state string, pr int) {
	t.Helper()
	ctx := context.Background()
	if _, err := rt.DB.ExecContext(ctx, `INSERT INTO runs(id,repository,issue_number,state,pr_number) VALUES(?,'owner/repo',7,?,?)`, id, state, pr); err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`{"run":{"id":%q,"repository":"owner/repo","issue_number":7,"pr_number":%d,"state":%q,"human_action":{"decision":"Choose","files":[],"options":["Retry"]}}}`, id, pr, state)
	if _, err := rt.DB.ExecContext(ctx, `INSERT INTO events(run_id,type,payload_json) VALUES(?,'run.needs_attention',?)`, id, payload); err != nil {
		t.Fatal(err)
	}
}

func commentFlow(t *testing.T, publisher *reportGitHub, now func() time.Time) (*scheduler.Scheduler, *app.Runtime, config.Config) {
	t.Helper()
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {{Number: 7, State: github.Open}}}, prs: map[string]*github.PullRequest{
		"feature": {Number: 101, State: github.Open},
	}, head: func(string) string { return "" }}
	_, rt := fixture(t, api)
	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.PRComments = true
	publisher.branchGitHub = branchGitHub{api}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: publisher, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return s, rt, cfg
}

func TestReportPublicationBacksOffTransientFailure(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	publisher := &reportGitHub{failure: true}
	s, rt, _ := commentFlow(t, publisher, func() time.Time { return now })
	seedReportPublication(t, rt, "report-transient", "MANUAL", 101)
	for range 2 {
		if _, err := s.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if publisher.calls != 1 {
		t.Fatalf("report retried before its backoff: %d calls", publisher.calls)
	}
	now = now.Add(time.Minute + time.Second)
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if publisher.calls != 2 {
		t.Fatalf("report did not retry after backoff: %d calls", publisher.calls)
	}
	var attempts int
	var stopped, failed bool
	if err := rt.DB.QueryRow(`SELECT attempts,failed,stopped FROM report_publications WHERE run_id='report-transient'`).Scan(&attempts, &failed, &stopped); err != nil || attempts != 2 || !failed || stopped {
		t.Fatalf("report retry state = %d %v %v, %v", attempts, failed, stopped, err)
	}
	if warnings := countEvents(t, rt, `SELECT count(*) FROM events WHERE run_id='report-transient' AND type='publication.warning'`); warnings != 1 {
		t.Fatalf("report warnings = %d, want one", warnings)
	}
}

func TestReportPublicationStopsOnPermanentFailure(t *testing.T) {
	publisher := &reportGitHub{failure: true, code: "github.invalid_input"}
	s, rt, _ := commentFlow(t, publisher, time.Now)
	seedReportPublication(t, rt, "report-permanent", "MANUAL", 101)
	for range 3 {
		if _, err := s.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if publisher.calls != 1 {
		t.Fatalf("permanent report failure retried: %d calls", publisher.calls)
	}
	var stopped bool
	var code, warning string
	if err := rt.DB.QueryRow(`SELECT stopped,warning_code,warning FROM report_publications WHERE run_id='report-permanent'`).Scan(&stopped, &code, &warning); err != nil || !stopped || code != "github.invalid_input" || !strings.Contains(warning, "cannot deliver") {
		t.Fatalf("permanent report state = %v %q %q, %v", stopped, code, warning, err)
	}
}

func TestFinishedRunStopsRetryingReportPublication(t *testing.T) {
	publisher := &reportGitHub{failure: true}
	s, rt, _ := commentFlow(t, publisher, time.Now)
	seedReportPublication(t, rt, "report-finished", "STOPPED", 101)
	for range 3 {
		if _, err := s.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if publisher.calls != 1 {
		t.Fatalf("finished report retried: %d calls", publisher.calls)
	}
	var stopped bool
	if err := rt.DB.QueryRow(`SELECT stopped FROM report_publications WHERE run_id='report-finished'`).Scan(&stopped); err != nil || !stopped {
		t.Fatalf("finished report was not stopped: %v, %v", stopped, err)
	}
	if warnings := countEvents(t, rt, `SELECT count(*) FROM events WHERE run_id='report-finished' AND type='publication.warning'`); warnings != 1 {
		t.Fatalf("finished report warnings = %d, want one", warnings)
	}
}

func TestAttentionPublicationBacksOffTransientFailure(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	publisher := &reportGitHub{failure: true}
	s, rt, _ := commentFlow(t, publisher, func() time.Time { return now })
	seedAttentionPublication(t, rt, "attention-transient", "MANUAL", 101)
	for range 2 {
		if _, err := s.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if publisher.calls != 1 {
		t.Fatalf("attention retried before its backoff: %d calls", publisher.calls)
	}
	now = now.Add(time.Minute + time.Second)
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if publisher.calls != 2 {
		t.Fatalf("attention did not retry after backoff: %d calls", publisher.calls)
	}
	var attempts int
	var stopped, failed bool
	if err := rt.DB.QueryRow(`SELECT attempts,failed,stopped FROM attention_publications WHERE run_id='attention-transient'`).Scan(&attempts, &failed, &stopped); err != nil || attempts != 2 || !failed || stopped {
		t.Fatalf("attention retry state = %d %v %v, %v", attempts, failed, stopped, err)
	}
}

func TestAttentionPublicationStopsOnPermanentFailure(t *testing.T) {
	publisher := &reportGitHub{failure: true, code: "github.invalid_input"}
	s, rt, _ := commentFlow(t, publisher, time.Now)
	seedAttentionPublication(t, rt, "attention-permanent", "MANUAL", 101)
	for range 3 {
		if _, err := s.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if publisher.calls != 1 {
		t.Fatalf("permanent attention failure retried: %d calls", publisher.calls)
	}
	var stopped bool
	if err := rt.DB.QueryRow(`SELECT stopped FROM attention_publications WHERE run_id='attention-permanent'`).Scan(&stopped); err != nil || !stopped {
		t.Fatalf("permanent attention was not stopped: %v, %v", stopped, err)
	}
	if warnings := countEvents(t, rt, `SELECT count(*) FROM events WHERE run_id='attention-permanent' AND type='publication.warning'`); warnings != 1 {
		t.Fatalf("permanent attention warnings = %d, want one", warnings)
	}
}

func TestFinishedRunStopsRetryingAttentionPublication(t *testing.T) {
	publisher := &reportGitHub{failure: true}
	s, rt, _ := commentFlow(t, publisher, time.Now)
	seedAttentionPublication(t, rt, "attention-finished", "STOPPED", 101)
	for range 3 {
		if _, err := s.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if publisher.calls != 1 {
		t.Fatalf("finished attention retried: %d calls", publisher.calls)
	}
	var stopped bool
	if err := rt.DB.QueryRow(`SELECT stopped FROM attention_publications WHERE run_id='attention-finished'`).Scan(&stopped); err != nil || !stopped {
		t.Fatalf("finished attention was not stopped: %v, %v", stopped, err)
	}
	if warnings := countEvents(t, rt, `SELECT count(*) FROM events WHERE run_id='attention-finished' AND type='publication.warning'`); warnings != 1 {
		t.Fatalf("finished attention warnings = %d, want one", warnings)
	}
}

func TestRunFinishingAfterTransientFailureStopsReportPublication(t *testing.T) {
	publisher := &reportGitHub{failure: true}
	s, rt, _ := commentFlow(t, publisher, time.Now)
	seedReportPublication(t, rt, "report-finishes-later", "MANUAL", 101)
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if publisher.calls != 1 {
		t.Fatalf("report attempts = %d, want 1", publisher.calls)
	}
	if _, err := rt.DB.Exec(`UPDATE runs SET state='STOPPED' WHERE id='report-finishes-later'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if publisher.calls != 1 {
		t.Fatalf("finished report retried: %d calls", publisher.calls)
	}
	var stopped bool
	var warning string
	if err := rt.DB.QueryRow(`SELECT stopped,warning FROM report_publications WHERE run_id='report-finishes-later'`).Scan(&stopped, &warning); err != nil || !stopped || !strings.Contains(warning, "will not retry") || !strings.Contains(warning, "No further retries") {
		t.Fatalf("finished report state = %v %q, %v", stopped, warning, err)
	}
	if err := rt.DB.QueryRow(`SELECT json_extract(payload_json,'$.message') FROM events WHERE type='publication.warning' AND json_extract(payload_json,'$.report_id')=(SELECT attempt_id FROM report_publications WHERE run_id='report-finishes-later') ORDER BY id DESC LIMIT 1`).Scan(&warning); err != nil || !strings.Contains(warning, "deferred") {
		t.Fatalf("report warning was rewritten: %q, %v", warning, err)
	}
	if err := rt.DB.QueryRow(`SELECT json_extract(payload_json,'$.message') FROM events WHERE type='publication.stopped' AND json_extract(payload_json,'$.report_id')=(SELECT attempt_id FROM report_publications WHERE run_id='report-finishes-later') ORDER BY id DESC LIMIT 1`).Scan(&warning); err != nil || !strings.Contains(warning, "will not retry") || !strings.Contains(warning, "No further retries") {
		t.Fatalf("finished report stop event = %q, %v", warning, err)
	}
}

func TestRunFinishingAfterTransientFailureStopsAttentionPublication(t *testing.T) {
	publisher := &reportGitHub{failure: true}
	s, rt, _ := commentFlow(t, publisher, time.Now)
	seedAttentionPublication(t, rt, "attention-finishes-later", "MANUAL", 101)
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if publisher.calls != 1 {
		t.Fatalf("attention attempts = %d, want 1", publisher.calls)
	}
	if _, err := rt.DB.Exec(`UPDATE runs SET state='STOPPED' WHERE id='attention-finishes-later'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if publisher.calls != 1 {
		t.Fatalf("finished attention retried: %d calls", publisher.calls)
	}
	var stopped bool
	if err := rt.DB.QueryRow(`SELECT stopped FROM attention_publications WHERE run_id='attention-finishes-later'`).Scan(&stopped); err != nil || !stopped {
		t.Fatalf("finished attention was not stopped: %v, %v", stopped, err)
	}
	var message string
	if err := rt.DB.QueryRow(`SELECT json_extract(payload_json,'$.message') FROM events WHERE type='publication.warning' AND json_extract(payload_json,'$.attention_event_id')=(SELECT event_id FROM attention_publications WHERE run_id='attention-finishes-later') ORDER BY id DESC LIMIT 1`).Scan(&message); err != nil || !strings.Contains(message, "deferred") {
		t.Fatalf("attention warning was rewritten: %q, %v", message, err)
	}
	if err := rt.DB.QueryRow(`SELECT json_extract(payload_json,'$.message') FROM events WHERE type='publication.stopped' AND json_extract(payload_json,'$.attention_event_id')=(SELECT event_id FROM attention_publications WHERE run_id='attention-finishes-later') ORDER BY id DESC LIMIT 1`).Scan(&message); err != nil || !strings.Contains(message, "will not retry") || !strings.Contains(message, "No further retries") {
		t.Fatalf("attention stop event = %q, %v", message, err)
	}
}

// ghRefusal is what gh reports for a refused request: the status line and a
// summary on stderr, and the response body on stdout.
type ghRefusal struct {
	stderr string
	stdout string
}

// ghComments answers the gh calls that publish a report comment: it lists the
// comments of an issue and creates one. A create can be refused with GitHub's
// own answer, so the scheduler classifies what GitHub actually said.
type ghComments struct {
	mu       sync.Mutex
	posts    int
	bodies   []string
	refusals []ghRefusal
}

func (g *ghComments) Run(_ context.Context, input []byte, args ...string) ([]byte, []byte, error) {
	method, endpoint := ghCall(args)
	switch {
	case method == "GET" && strings.HasSuffix(endpoint, "/comments"):
		return []byte("[]"), nil, nil
	case method == "POST" && strings.HasSuffix(endpoint, "/comments"):
		g.mu.Lock()
		defer g.mu.Unlock()
		g.posts++
		if len(g.refusals) > 0 {
			refusal := g.refusals[0]
			g.refusals = g.refusals[1:]
			return []byte(refusal.stdout), []byte(refusal.stderr), errors.New("exit status 1")
		}
		var payload struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal(input, &payload); err != nil {
			return nil, nil, err
		}
		g.bodies = append(g.bodies, payload.Body)
		comment, err := json.Marshal(map[string]any{
			"id":       len(g.bodies),
			"html_url": fmt.Sprintf("https://github.com/owner/repo/pull/101#issuecomment-%d", len(g.bodies)),
			"body":     payload.Body,
		})
		return comment, nil, err
	}
	return nil, nil, fmt.Errorf("unexpected gh call: %v", args)
}

// published reports how many comments were created and their bodies.
func (g *ghComments) published() (int, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.posts, append([]string(nil), g.bodies...)
}

// ghCall reads the method and the endpoint of one gh invocation.
func ghCall(args []string) (string, string) {
	method, endpoint := "", ""
	for i, arg := range args {
		switch {
		case arg == "--method" && i+1 < len(args):
			method = args[i+1]
		case strings.HasPrefix(arg, "repos/"):
			endpoint = strings.SplitN(arg, "?", 2)[0]
		}
	}
	return method, endpoint
}

// ghPublisher publishes report comments through the gh adapter, so the
// scheduler sees GitHub's own answers instead of a prepared code.
type ghPublisher struct {
	branchGitHub
	client *github.Client
}

func (p *ghPublisher) EnsureReportComment(ctx context.Context, repository string, number int, body string) (*github.Comment, error) {
	return p.client.EnsureReportComment(ctx, repository, number, body)
}

// ghCommentFlow runs a scheduler whose report comments travel through the gh
// adapter and the given runner.
func ghCommentFlow(t *testing.T, runner *ghComments, now func() time.Time) (*scheduler.Scheduler, *app.Runtime) {
	t.Helper()
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {{Number: 7, State: github.Open}}}, prs: map[string]*github.PullRequest{
		"feature": {Number: 101, State: github.Open},
	}, head: func(string) string { return "" }}
	_, rt := fixture(t, api)
	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.PRComments = true
	publisher := &ghPublisher{branchGitHub: branchGitHub{api}, client: github.New(runner)}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: publisher, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return s, rt
}

func TestThrottledReportCommentBacksOffAndIsDelivered(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	runner := &ghComments{refusals: []ghRefusal{{
		stderr: "gh: Validation failed, or the endpoint has been spammed (HTTP 422)",
		stdout: `{"message": "Validation failed, or the endpoint has been spammed"}`,
	}}}
	s, rt := ghCommentFlow(t, runner, func() time.Time { return now })
	seedReportPublication(t, rt, "report-throttled", "MANUAL", 101)
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
	if posts, _ := runner.published(); posts != 1 {
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
	posts, bodies := runner.published()
	if posts != 2 || len(bodies) != 1 {
		t.Fatalf("published comments = %d attempts, %d bodies", posts, len(bodies))
	}
}

func TestPermanentRefusalStopsPublicationWithWarning(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refusal ghRefusal
	}{
		{"oversized body", ghRefusal{
			stderr: "gh: Body is too long (maximum is 65536 characters) (HTTP 422)",
			stdout: `{"message": "Body is too long (maximum is 65536 characters)"}`,
		}},
		{"invalid field", ghRefusal{
			stderr: "gh: Validation Failed (HTTP 422)",
			stdout: `{"message": "Validation Failed", "errors": [{"resource": "IssueComment", "field": "body", "code": "missing"}]}`,
		}},
		{"nested size error", ghRefusal{
			stderr: "gh: Validation Failed (HTTP 422)",
			stdout: `{"message": "Validation Failed", "errors": [{"resource": "IssueComment", "field": "body", "code": "custom", "message": "body is too long (maximum is 65536 characters)"}]}`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &ghComments{refusals: []ghRefusal{tc.refusal}}
			s, rt := ghCommentFlow(t, runner, time.Now)
			seedReportPublication(t, rt, "report-permanent", "MANUAL", 101)
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
			if posts, _ := runner.published(); posts != 1 {
				t.Fatalf("permanent publication retried: %d", posts)
			}
		})
	}
}
