package scheduler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/web"
)

// The dashboard HTTP surface carries the confirmation markup and loading state
// for every destructive or quota-consuming action.
func TestRunDetailHTTPConfirmsActionsAndShowsLoading(t *testing.T) {
	runtime, err := app.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close() })
	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
	if err != nil {
		t.Fatal(err)
	}
	h := &creditHarness{classifiedHarness: &classifiedHarness{HarnessAdapter: harness.NewClaude(cfg.Agents.Claude)}, detection: true}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{
		GitHub: &fakeGitHub{}, Harnesses: map[string]harness.HarnessAdapter{"claude": h},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.DB.Exec(`INSERT INTO runs(id,repository,issue_number,state,current_phase,worktree_path,implementer_agent,implementer_session_id,updated_at)
		VALUES('run-actions','owner/repo',7,'ACTIVE','implement','/tmp/worktree','claude','session-1','2026-10-21T09:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.DB.Exec(`INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status,session_id)
		VALUES('attempt-1','run-actions','implement','implementer',0,1,'claude','running','session-1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.DB.Exec(`INSERT INTO harness_limits(harness_type,reason,restriction_id) VALUES('claude','credits_exhausted','restriction-1')`); err != nil {
		t.Fatal(err)
	}
	server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{
		DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg,
	})
	if err != nil {
		t.Fatal(err)
	}

	page := httptest.NewRecorder()
	server.ServeHTTP(page, dashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/run-actions", nil))
	body := page.Body.String()
	if page.Code != http.StatusOK {
		t.Fatalf("run detail = %d: %s", page.Code, body)
	}
	for _, want := range []string{
		`data-confirm="#confirm-stop"`, `id="confirm-stop"`, "interrupts the current phase",
		`data-confirm="#confirm-takeover"`, `id="confirm-takeover"`, "suspends automation",
		`data-confirm="#confirm-check-claude"`, `id="confirm-check-claude"`, "uses the claude account quota",
		`action="/runs/run-actions/stop"`, `action="/runs/run-actions/takeover"`, `action="/harnesses/claude/check"`,
		`name="csrf_token"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("run detail is missing %q", want)
		}
	}
	for _, form := range []string{"/runs/run-actions/stop", "/runs/run-actions/takeover", "/harnesses/claude/check"} {
		at := strings.Index(body, `action="`+form+`"`)
		if at < 0 {
			t.Fatalf("no form posts to %s", form)
		}
		start := strings.LastIndex(body[:at], "<form")
		end := strings.Index(body[at:], "</form>")
		if start < 0 || end < 0 {
			t.Fatalf("malformed form for %s", form)
		}
		markup := body[start : at+end+len("</form>")]
		for _, want := range []string{`method="post"`, "hx-disabled-elt=", "htmx-indicator", `type="submit"`} {
			if !strings.Contains(markup, want) {
				t.Errorf("form for %s is missing %q: %s", form, want, markup)
			}
		}
	}

	dashboard := httptest.NewRecorder()
	server.ServeHTTP(dashboard, dashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/", nil))
	if dashboard.Code != http.StatusOK || !strings.Contains(dashboard.Body.String(), `action="/scheduler/pause"`) ||
		!strings.Contains(dashboard.Body.String(), "hx-disabled-elt=") || !strings.Contains(dashboard.Body.String(), "htmx-indicator") {
		t.Fatalf("scheduler action has no loading state: %d %s", dashboard.Code, dashboard.Body.String())
	}
}
