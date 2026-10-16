package scheduler_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/web"
)

func TestDashboardObservationKeepsBoundedSelectionAndOlderDetail(t *testing.T) {
	s, rt := configured(t, &fakeGitHub{}, "repositories:\n  - repo: owner/repo\n", rejectingGit{})
	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 25; i++ {
		// Tied timestamps exercise the stable ID ordering; old malformed evidence
		// makes loading all historical runs fail instead of silently passing.
		roleSettings := "null"
		if i > 20 {
			roleSettings = "invalid"
		}
		_, err := rt.DB.Exec(`INSERT INTO runs(id,repository,issue_number,state,updated_at,role_settings_json) VALUES (?,'owner/repo',?,'STOPPED','2026-10-15T10:00:00Z',?)`, fmt.Sprintf("terminal-%02d", i), i, roleSettings)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rt.DB.Exec(`INSERT INTO runs(id,repository,issue_number,state,updated_at) VALUES ('active-run','owner/repo',99,'PREPARING','2026-10-14T10:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	page := httptest.NewRecorder()
	server.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/", nil))
	body := page.Body.String()
	if page.Code != 200 || !strings.Contains(body, "/runs/active-run") {
		t.Fatalf("active run missing: %d %s", page.Code, body)
	}
	previous := -1
	for i := 1; i <= 25; i++ {
		position := strings.Index(body, fmt.Sprintf("/runs/terminal-%02d", i))
		if i <= 20 {
			if position < 0 || position <= previous {
				t.Fatalf("terminal %d missing or out of order", i)
			}
			previous = position
		} else if position >= 0 {
			t.Fatalf("unbounded terminal selection includes %d", i)
		}
	}
	// A selected older run remains addressable outside the recent window.
	if _, err := rt.DB.ExecContext(context.Background(), `UPDATE runs SET role_settings_json=NULL WHERE id='terminal-25'`); err != nil {
		t.Fatal(err)
	}
	detail := httptest.NewRecorder()
	server.ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/runs/terminal-25", nil))
	if detail.Code != 200 || !strings.Contains(detail.Body.String(), "terminal-25") {
		t.Fatalf("older selected run missing: %d %s", detail.Code, detail.Body.String())
	}
}
