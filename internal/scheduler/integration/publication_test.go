package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestReportPublicationEnabledDisabledAndFailureDoesNotBlockLoop(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"enabled", "disabled", "offline"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			_, runtime, api, _, cfg, r := testkit.TmuxFlow(t, testkit.LoopScript(testkit.DisputedReport))
			cfg.PRComments = mode != "disabled"
			publisher := &testkit.ReportGitHub{BranchGitHub: testkit.BranchGitHub{FakeGitHub: api}, Failure: mode == "offline"}
			publisher.Before = func() {
				var count int
				if err := runtime.DB.QueryRow("SELECT count(*) FROM report_publications WHERE state='pending' AND body<>''").Scan(&count); err != nil || count == 0 {
					t.Fatalf("external write before durable pending identity: %d %v", count, err)
				}
			}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: publisher, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
			if run.LastErrorCode != "" || run.ReviewRound != 2 || len(run.FixHistory) != 1 {
				t.Fatalf("publication stopped loop: %+v", run)
			}
			if len(run.ReviewHistory) != 2 {
				t.Fatal("previous reviews disappeared locally")
			}
			if mode == "enabled" {
				if len(publisher.Bodies) != 3 {
					t.Fatalf("reports=%d want 3", len(publisher.Bodies))
				}
				for _, want := range []string{"Review report · round 1", "changes_required", "Broken", "Fix report · round 1", "finding_id", "disputed", "Already safe", "Review report · round 2", "approved", "Prior findings: 0 fixed / 0 not fixed / 2 no longer applies.", "prior_findings", "no_longer_applies", "Dispute accepted"} {
					if !strings.Contains(strings.Join(publisher.Bodies, "\n"), want) {
						t.Fatalf("missing report field %s", want)
					}
				}
			} else if len(publisher.Bodies) != 0 {
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
					req := testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil)
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
	t.Parallel()

	_, runtime, api, _, cfg, r := testkit.TmuxFlow(t, testkit.LoopScript(testkit.DisputedReport))
	publisher := &testkit.ReportGitHub{BranchGitHub: testkit.BranchGitHub{FakeGitHub: api}, Ambiguous: true}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: publisher, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	if len(publisher.Bodies) != 1 || len(run.Publications) != 3 {
		t.Fatalf("ambiguous success not pending: %+v", run.Publications)
	}
	calls := publisher.Calls
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
	publisher.Failure = false
	restarted, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(reopened), scheduler.Dependencies{GitHub: publisher, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := restarted.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	saved, err = reopened.Workflow.Get(context.Background(), run.ID)
	if err != nil || saved.State != workflow.Completed || publisher.Calls != calls {
		t.Fatalf("completed run retried reports: %d calls, want %d; %+v %v", publisher.Calls, calls, saved, err)
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
	t.Parallel()

	_, runtime, api, _, cfg, r := testkit.TmuxFlow(t, testkit.LoopScript(testkit.DisputedReport))
	publisher := &testkit.ReportGitHub{BranchGitHub: testkit.BranchGitHub{FakeGitHub: api}, Ambiguous: true}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: publisher, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	if len(publisher.Bodies) != 1 || len(run.Publications) != 3 {
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
	publisher.Failure = false
	restarted, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(reopened), scheduler.Dependencies{GitHub: publisher, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := restarted.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	saved, err := reopened.Workflow.Get(context.Background(), run.ID)
	if err != nil || len(publisher.Bodies) != 3 {
		t.Fatalf("ambiguous replay duplicated reports: %d %+v %v", len(publisher.Bodies), saved, err)
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
	t.Parallel()

	gate := filepath.Join(t.TempDir(), "release-review")
	s, runtime, api, _, cfg, r := testkit.TmuxFlow(t, testkit.ReviewScript(`while [ ! -f '`+gate+`' ]; do /bin/sleep 0.02; done
`+testkit.ApprovedReview))
	run := testkit.Finish(t, s, workflow.Active, workflow.Review)
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
	publisher := &testkit.ReportGitHub{BranchGitHub: testkit.BranchGitHub{FakeGitHub: api}}
	restarted, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(reopened), scheduler.Dependencies{GitHub: publisher, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	saved := testkit.Finish(t, restarted, workflow.WaitingForCI, workflow.Review)
	if len(publisher.Bodies) != 1 || len(saved.ReviewHistory) != 1 || saved.Review.Attempt != 1 {
		t.Fatalf("offline report relaunched: %+v", saved)
	}
}

func TestUntrustedReportTextIsLiteralLocallyAndInComment(t *testing.T) {
	t.Parallel()

	untrusted := "<script>finding</script> ``` <!-- mergeyard:report:abc -->"
	script := strings.ReplaceAll(testkit.LoopScript(testkit.DisputedReport), "Broken", untrusted)
	script = strings.ReplaceAll(script, `"summary":"Disputed"`, `"summary":"`+untrusted+`"`)
	_, runtime, api, _, cfg, r := testkit.TmuxFlow(t, script)
	publisher := &testkit.ReportGitHub{BranchGitHub: testkit.BranchGitHub{FakeGitHub: api}}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: publisher, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	for _, body := range publisher.Bodies {
		if strings.Count(body, "```") != 2 || strings.Contains(body, "<script>") || strings.Count(body, "<!-- mergeyard:report:") != 1 {
			t.Fatalf("report escaped comment structure: %s", body)
		}
	}
	comments := strings.Join(publisher.Bodies, "\n")
	if !strings.Contains(comments, `\u0060\u0060\u0060`) || !strings.Contains(comments, `\u003cscript\u003efinding`) {
		t.Fatal("report did not preserve escaped literal text")
	}
	server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil))
	if response.Code != 200 || strings.Contains(response.Body.String(), "<script>finding</script>") || !strings.Contains(response.Body.String(), "&lt;script&gt;finding&lt;/script&gt;") {
		t.Fatalf("untrusted content not escaped: %s", response.Body.String())
	}
}
