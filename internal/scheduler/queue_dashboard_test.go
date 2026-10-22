package scheduler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// seedQueueRun writes one run directly: this test is about where the dashboard
// presents lifecycle states, not how a run reached them.
func seedQueueRun(t *testing.T, runtime *app.Runtime, id, repository string, number int, state workflow.State, phase workflow.Phase, pr int, updated string) {
	t.Helper()
	if _, err := runtime.DB.Exec(`INSERT INTO runs(id,repository,issue_number,state,current_phase,review_round,pr_number,updated_at)
 VALUES(?,?,?,?,?,0,?,?)`, id, repository, number, state, phase, pr, updated); err != nil {
		t.Fatal(err)
	}
}

// queueGroupBody returns the table rows from one group header to the next.
func queueGroupBody(t *testing.T, body, group string) string {
	t.Helper()
	start := strings.Index(body, `id="group-`+group+`"`)
	if start < 0 {
		t.Fatalf("queue has no group %q", group)
	}
	end := len(body)
	if offset := strings.Index(body[start+1:], `id="group-`); offset >= 0 {
		end = start + 1 + offset
	}
	return body[start:end]
}

// The dashboard HTTP surface places every open state and queued issue in its
// documented group exactly once, hides ended runs, and keeps the concurrency
// count unchanged.
func TestDashboardQueuePlacesStatesAndIssuesInSixGroups(t *testing.T) {
	attention := ready(202)
	attention.Labels = append(attention.Labels, github.Label{Name: "agent-needs-attention"})
	api := &fakeGitHub{
		issues:   map[string][]github.Issue{"owner/one": {}, "owner/two": {ready(201), attention, ready(203)}},
		blockers: map[int][]github.Issue{203: {ready(99)}},
		listError: func(repo string) error {
			if repo == "owner/one" {
				return &fault.Error{Code: "github.rate_limited", Message: "GitHub rate limit reached."}
			}
			return nil
		},
	}
	s, runtime := configured(t, api, "repositories:\n  - repo: owner/one\n  - repo: owner/two\n", rejectingGit{})
	seedQueueRun(t, runtime, "run-claiming", "owner/one", 101, workflow.Claiming, "", 0, "2026-10-21T09:01:00Z")
	seedQueueRun(t, runtime, "run-preparing", "owner/one", 113, workflow.Preparing, "", 0, "2026-10-21T09:01:30Z")
	seedQueueRun(t, runtime, "run-active", "owner/one", 102, workflow.Active, workflow.Implement, 0, "2026-10-21T09:02:00Z")
	seedQueueRun(t, runtime, "run-review-pending", "owner/one", 103, workflow.Active, workflow.Review, 42, "2026-10-21T09:03:00Z")
	seedQueueRun(t, runtime, "run-ci", "owner/one", 104, workflow.WaitingForCI, workflow.Review, 43, "2026-10-21T09:04:00Z")
	seedQueueRun(t, runtime, "run-manual", "owner/one", 105, workflow.Manual, workflow.Implement, 0, "2026-10-21T09:05:00Z")
	seedQueueRun(t, runtime, "run-harness", "owner/one", 106, workflow.WaitingForHarness, workflow.Review, 44, "2026-10-21T09:06:00Z")
	seedQueueRun(t, runtime, "run-merge", "owner/one", 107, workflow.ReadyToMerge, workflow.Review, 45, "2026-10-21T09:07:00Z")
	seedQueueRun(t, runtime, "run-attention", "owner/one", 108, workflow.NeedsAttention, workflow.Fix, 0, "2026-10-21T09:08:00Z")
	seedQueueRun(t, runtime, "run-cleanup", "owner/one", 109, workflow.Completed, workflow.Review, 46, "2026-10-21T09:09:00Z")
	if _, err := runtime.DB.Exec(`INSERT INTO merge_cleanup(run_id,snapshot_json) VALUES(?,?)`, "run-cleanup", `{"observed_at":"2026-10-21T10:00:00Z"}`); err != nil {
		t.Fatal(err)
	}
	seedQueueRun(t, runtime, "run-failed", "owner/one", 110, workflow.Failed, workflow.Implement, 0, "2026-10-21T09:10:00Z")
	seedQueueRun(t, runtime, "run-stopped", "owner/one", 111, workflow.Stopped, workflow.Implement, 0, "2026-10-21T09:11:00Z")
	seedQueueRun(t, runtime, "run-completed", "owner/one", 112, workflow.Completed, workflow.Review, 47, "2026-10-21T09:12:00Z")

	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/one\n  - repo: owner/two\n"))
	if err != nil {
		t.Fatal(err)
	}
	server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{
		DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); server.RunUpdates(ctx) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(3 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		page := httptest.NewRecorder()
		server.ServeHTTP(page, dashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/queue", nil))
		body = page.Body.String()
		if strings.Contains(body, "#201") {
			if page.Code != http.StatusOK {
				t.Fatalf("queue: %d %s", page.Code, body)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(body, "#201") {
		t.Fatal("queue discovery never rendered the ready issue")
	}

	for group, runs := range map[string][]string{
		"attention": {"run-attention", "run-cleanup"},
		"merge":     {"run-merge"},
		"running":   {"run-claiming", "run-preparing", "run-active", "run-review-pending", "run-ci", "run-manual"},
		"harness":   {"run-harness"},
	} {
		segment := queueGroupBody(t, body, group)
		for _, id := range runs {
			if !strings.Contains(segment, `href="/runs/`+id+`"`) {
				t.Errorf("%s group lacks run %q: %s", group, id, segment)
			}
			if count := strings.Count(body, `href="/runs/`+id+`"`); count != 1 {
				t.Errorf("run %q appears %d times in the queue, want 1", id, count)
			}
		}
	}
	if segment := queueGroupBody(t, body, "attention"); !strings.Contains(segment, "Cleanup pending") {
		t.Errorf("merged run with pending cleanup does not explain itself: %s", segment)
	}
	if segment := queueGroupBody(t, body, "attention"); !strings.Contains(segment, "GitHub failure") || !strings.Contains(segment, "github.rate_limited") {
		t.Errorf("scoped GitHub failure is not a Needs attention row: %s", segment)
	}
	if segment := queueGroupBody(t, body, "running"); !strings.Contains(segment, ">Review queued<") {
		t.Errorf("draft PR waiting for review is not labelled Review queued: %s", segment)
	}
	if segment := queueGroupBody(t, body, "ready"); !strings.Contains(segment, "#201") {
		t.Errorf("Ready group lacks the eligible issue: %s", segment)
	}
	if segment := queueGroupBody(t, body, "attention"); !strings.Contains(segment, "#202") {
		t.Errorf("Needs attention group lacks the flagged issue: %s", segment)
	}
	if segment := queueGroupBody(t, body, "blocked"); !strings.Contains(segment, "#203") || !strings.Contains(segment, "Blocked by") {
		t.Errorf("Blocked group lacks the blocked issue: %s", segment)
	}
	for _, id := range []string{"run-failed", "run-stopped", "run-completed"} {
		if strings.Contains(body, `href="/runs/`+id+`"`) {
			t.Errorf("ended run %q is still a queue item", id)
		}
	}
	if strings.Contains(body, "Pause scheduler") || strings.Contains(body, "8 / 1") {
		t.Errorf("Queue still carries the Dashboard's overview controls: %s", body)
	}
	if !strings.Contains(body, ">Repository<") {
		t.Errorf("two configured repositories do not show the repository column: %s", body)
	}
	dashboard := httptest.NewRecorder()
	server.ServeHTTP(dashboard, dashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/", nil))
	if dashboard.Code != http.StatusOK || !strings.Contains(dashboard.Body.String(), "8 / 1") {
		t.Errorf("Dashboard lost the concurrency slots: %d %s", dashboard.Code, dashboard.Body.String())
	}
}
