package scheduler_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/ci"
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
	ambiguous bool
	before    func()
}

func (f *reportGitHub) EnsureReportComment(_ context.Context, _ string, _ int, body string) (*github.Comment, error) {
	if f.before != nil {
		f.before()
	}
	if f.failure {
		return nil, &fault.Error{Code: "github.unavailable", Message: "secret token <script> unsafe upstream diagnostic"}
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
				for _, want := range []string{"Review report · round 1", "changes_required", "Broken", "Fix report · round 1", "finding_id", "disputed", "Already safe", "Review report · round 2", "approved"} {
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
					req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil)
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

func TestCompletedRunPublicationReplaysAfterLostSuccessAndRuntimeRestart(t *testing.T) {
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
	if err != nil || saved.State != workflow.Completed || len(publisher.bodies) != 3 {
		t.Fatalf("completed replay duplicated reports: %d %+v %v", len(publisher.bodies), saved, err)
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
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil))
	if response.Code != 200 || strings.Contains(response.Body.String(), "<script>finding</script>") || !strings.Contains(response.Body.String(), "&lt;script&gt;finding&lt;/script&gt;") {
		t.Fatalf("untrusted content not escaped: %s", response.Body.String())
	}
}

// Retain the publishing boundary while exercising the CI boundary on the same PR.
type publishingCIGitHub struct {
	*reportGitHub
	checks *ciGitHub
}

func (f publishingCIGitHub) CheckEvidence(ctx context.Context, repo, sha, base string) (ci.Evidence, error) {
	return f.checks.CheckEvidence(ctx, repo, sha, base)
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
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil))
	for _, want := range []string{"Waiting for your merge", "CI and readiness", "Wait deadline", "Publication warning", "github.unavailable"} {
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), want) {
			t.Fatalf("missing %q from combined detail", want)
		}
	}
	response = httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/static/app.js", nil))
	for _, event := range []string{"publication.pending", "publication.warning", "publication.published", "ci.updated", "pr.readiness_started"} {
		if !strings.Contains(response.Body.String(), event) {
			t.Fatalf("missing live update %q", event)
		}
	}
}
