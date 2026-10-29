package scheduler_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	// Run detail is one scrolling page at the dashboard HTTP surface: a sticky
	// header, the lifecycle stepper, the current area, and collapsible history for
	// every state the scheduler persists.
)

func TestRunDetailHTTPRendersHeaderStepperCurrentAndHistoryForEveryState(t *testing.T) {
	t.Parallel()

	s, rt := testkit.Configured(t, &testkit.FakeGitHub{}, "repositories:\n  - repo: owner/repo\n", testkit.RejectingGit{})
	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		id, state, Phase, step string
		round                  int
	}{
		{"claiming", "CLAIMING", "", "Claimed", 0},
		{"preparing", "PREPARING", "", "Claimed", 0},
		{"implementing", "ACTIVE", "implement", "Implement", 0},
		{"reviewing", "ACTIVE", "review", "Review · round 2", 2},
		{"fixing", "ACTIVE", "fix", "Fix", 2},
		{"waiting-ci", "WAITING_FOR_CI", "review", "CI", 1},
		{"waiting-harness", "WAITING_FOR_HARNESS", "review", "Review · round 1", 1},
		{"manual", "MANUAL", "implement", "Implement", 0},
		{"attention", "NEEDS_ATTENTION", "review", "Review · round 1", 1},
		{"ready", "READY_TO_MERGE", "review", "Ready", 1},
		{"completed", "COMPLETED", "review", "Merged", 1},
		{"failed", "FAILED", "implement", "Implement", 0},
		{"stopped", "STOPPED", "", "Claimed", 0},
	}
	for i, tc := range cases {
		if _, err := rt.DB.Exec(`INSERT INTO runs(id,repository,issue_number,state,current_phase,review_round) VALUES (?, 'owner/repo', ?, ?, ?, ?)`,
			tc.id, 100+i, tc.state, tc.Phase, tc.round); err != nil {
			t.Fatal(err)
		}
	}
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	current := regexp.MustCompile(`<li class="step[^"]*" aria-current="step"><span[^>]*>([^<]+)</span></li>`)
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			page := httptest.NewRecorder()
			server.ServeHTTP(page, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/"+tc.id, nil))
			body := page.Body.String()
			if page.Code != http.StatusOK {
				t.Fatalf("run detail = %d: %s", page.Code, body)
			}
			for _, want := range []string{`<header id="run-header"`, "sticky", `aria-label="Run lifecycle"`, `id="run-current"`, `id="history-heading"`, `id="run-facts"`} {
				if !strings.Contains(body, want) {
					t.Errorf("run detail is missing %q: %s", want, body)
				}
			}
			match := current.FindStringSubmatch(body)
			if match == nil || match[1] != tc.step {
				t.Errorf("current step = %q, want %q", match, tc.step)
			}
			if strings.Contains(body, fmt.Sprintf(">%s<", tc.state)) {
				t.Errorf("run detail renders the raw state %s", tc.state)
			}
		})
	}
}
