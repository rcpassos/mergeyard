package scheduler_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

var (
	kpiValuePattern    = regexp.MustCompile(`id="kpi-([a-z]+)">([^<]+)<`)
	groupCountPattern  = regexp.MustCompile(`id="group-([a-z]+)"[^>]*>[^<]*<span[^>]*>(\d+)</span>`)
	dashboardURLPrefix = "http://127.0.0.1:7331"
)

// dashboardBody serves one dashboard page for the overview tests.
func dashboardBody(t *testing.T, server *web.Server, path string) string {
	t.Helper()
	page := httptest.NewRecorder()
	server.ServeHTTP(page, dashboardRequest(t, server, http.MethodGet, dashboardURLPrefix+path, nil))
	if page.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", path, page.Code, page.Body.String())
	}
	return page.Body.String()
}

func kpis(t *testing.T, body string) map[string]string {
	t.Helper()
	values := map[string]string{}
	for _, match := range kpiValuePattern.FindAllStringSubmatch(body, -1) {
		values[match[1]] = strings.TrimSpace(match[2])
	}
	return values
}

func queueGroupCounts(t *testing.T, body string) map[string]string {
	t.Helper()
	counts := map[string]string{}
	for _, match := range groupCountPattern.FindAllStringSubmatch(body, -1) {
		counts[match[1]] = match[2]
	}
	return counts
}

// section returns the markup from start to the next end marker.
func section(t *testing.T, body, start, end string) string {
	t.Helper()
	at := strings.Index(body, start)
	if at < 0 {
		t.Fatalf("page lacks %q", start)
	}
	body = body[at:]
	if next := strings.Index(body, end); next >= 0 {
		body = body[:next]
	}
	return body
}

// The Dashboard is an overview: its KPIs read the same Queue groups, and Needs
// you lists exactly the Needs attention and Waiting for your merge items.
func TestDashboardOverviewKPIsNeedsYouAndQueueCountsAgree(t *testing.T) {
	attention := ready(202)
	attention.Labels = append(attention.Labels, github.Label{Name: "agent-needs-attention"})
	api := &fakeGitHub{
		issues:   map[string][]github.Issue{"owner/repo": {ready(201), attention, ready(203)}},
		blockers: map[int][]github.Issue{203: {ready(99)}},
	}
	s, rt := configured(t, api, "repositories:\n  - repo: owner/repo\n", rejectingGit{})
	seedQueueRun(t, rt, "run-active", "owner/repo", 101, workflow.Active, workflow.Implement, 0, "2026-10-21T09:02:00Z")
	seedQueueRun(t, rt, "run-attention", "owner/repo", 102, workflow.NeedsAttention, workflow.Fix, 0, "2026-10-21T09:03:00Z")
	seedQueueRun(t, rt, "run-merge", "owner/repo", 103, workflow.ReadyToMerge, workflow.Review, 41, "2026-10-21T09:04:00Z")
	seedQueueRun(t, rt, "run-harness", "owner/repo", 104, workflow.WaitingForHarness, workflow.Review, 42, "2026-10-21T09:05:00Z")
	seedQueueRun(t, rt, "run-cleanup", "owner/repo", 105, workflow.Completed, workflow.Review, 43, "2026-10-21T09:06:00Z")
	if _, err := rt.DB.Exec(`INSERT INTO merge_cleanup(run_id,snapshot_json) VALUES(?,?)`, "run-cleanup", `{"observed_at":"2026-10-21T10:00:00Z"}`); err != nil {
		t.Fatal(err)
	}
	seedQueueRun(t, rt, "run-ended", "owner/repo", 106, workflow.Failed, workflow.Implement, 0, "2026-10-21T08:00:00Z")

	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
	if err != nil {
		t.Fatal(err)
	}
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); server.RunUpdates(ctx) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(3 * time.Second)
	var queueBody string
	for time.Now().Before(deadline) {
		queueBody = dashboardBody(t, server, "/queue")
		if strings.Contains(queueBody, "#201") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(queueBody, "#201") {
		t.Fatal("queue discovery never rendered the ready issue")
	}
	dashboard := dashboardBody(t, server, "/")

	counts := queueGroupCounts(t, queueBody)
	kpi := kpis(t, dashboard)
	for _, key := range []string{"running", "attention", "merge"} {
		if kpi[key] != counts[key] {
			t.Errorf("KPI %s = %q, Queue group count = %q", key, kpi[key], counts[key])
		}
	}
	if counts["running"] != "1" || counts["attention"] != "3" || counts["merge"] != "1" {
		t.Errorf("Queue counts = %v, want running 1, attention 3, merge 1", counts)
	}
	if kpi["concurrency"] != "3 / 1" {
		t.Errorf("concurrency KPI = %q, want 3 / 1", kpi["concurrency"])
	}

	needsYou := section(t, dashboard, `id="needs-you-heading"`, `id="recently-ended-heading"`)
	for _, want := range []string{`href="/runs/run-attention"`, `href="/runs/run-cleanup"`, `href="/runs/run-merge"`, "#202"} {
		if !strings.Contains(needsYou, want) {
			t.Errorf("Needs you lacks %q: %s", want, needsYou)
		}
	}
	for _, unwanted := range []string{`href="/runs/run-active"`, `href="/runs/run-harness"`, `href="/runs/run-ended"`, "#201", "#203"} {
		if strings.Contains(needsYou, unwanted) {
			t.Errorf("Needs you includes %q: %s", unwanted, needsYou)
		}
	}
	if !strings.Contains(needsYou, `id="group-attention"`) || !strings.Contains(needsYou, `id="group-merge"`) {
		t.Errorf("Needs you does not use the Queue group rows: %s", needsYou)
	}
	// The Dashboard no longer duplicates the full Queue.
	for _, key := range []string{"running", "harness", "ready", "blocked"} {
		if strings.Contains(dashboard, `id="group-`+key+`"`) {
			t.Errorf("Dashboard still renders the full %s group: %s", key, dashboard)
		}
	}
	if !strings.Contains(dashboard, "Pause scheduler") {
		t.Error("Dashboard lost the scheduler control")
	}
}

// With nothing waiting on the owner, Needs you is a positive all-clear.
func TestDashboardNeedsYouAllClear(t *testing.T) {
	s, rt := configured(t, &fakeGitHub{}, "repositories:\n  - repo: owner/repo\n", rejectingGit{})
	seedQueueRun(t, rt, "run-active", "owner/repo", 101, workflow.Active, workflow.Implement, 0, "2026-10-21T09:02:00Z")
	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
	if err != nil {
		t.Fatal(err)
	}
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	dashboard := dashboardBody(t, server, "/")
	kpi := kpis(t, dashboard)
	if kpi["attention"] != "0" || kpi["merge"] != "0" || kpi["running"] != "1" {
		t.Errorf("KPIs = %v, want attention 0, merge 0, running 1", kpi)
	}
	needsYou := section(t, dashboard, `id="needs-you-heading"`, `id="recently-ended-heading"`)
	if !strings.Contains(needsYou, "All clear") {
		t.Errorf("Needs you lacks the all-clear state: %s", needsYou)
	}
	if strings.Contains(needsYou, `id="group-`) {
		t.Errorf("all-clear still renders empty groups: %s", needsYou)
	}
}

// The Dashboard shows at most ten ended runs and links to the full history,
// which lists every ended run and leaves open work out.
func TestDashboardRecentlyEndedLinksToFullHistory(t *testing.T) {
	s, rt := configured(t, &fakeGitHub{}, "repositories:\n  - repo: owner/repo\n", rejectingGit{})
	for i := 1; i <= 12; i++ {
		seedQueueRun(t, rt, fmt.Sprintf("ended-%02d", i), "owner/repo", 100+i, workflow.Failed, workflow.Implement, 0,
			fmt.Sprintf("2026-10-%02dT10:00:00Z", i))
	}
	seedQueueRun(t, rt, "ended-pending", "owner/repo", 120, workflow.Completed, workflow.Review, 50, "2026-10-20T10:00:00Z")
	if _, err := rt.DB.Exec(`INSERT INTO merge_cleanup(run_id,snapshot_json) VALUES(?,?)`, "ended-pending", `{"observed_at":"2026-10-21T10:00:00Z"}`); err != nil {
		t.Fatal(err)
	}
	seedQueueRun(t, rt, "active-run", "owner/repo", 121, workflow.Active, workflow.Implement, 0, "2026-10-21T10:00:00Z")
	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
	if err != nil {
		t.Fatal(err)
	}
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	dashboard := dashboardBody(t, server, "/")
	if !strings.Contains(dashboard, `href="/history"`) {
		t.Error("Recently ended has no full-history link")
	}
	recent := section(t, dashboard, `id="recently-ended-heading"`, "</main>")
	for i := 3; i <= 12; i++ {
		if !strings.Contains(recent, fmt.Sprintf(`href="/runs/ended-%02d"`, i)) {
			t.Errorf("Recently ended lacks ended-%02d", i)
		}
	}
	for _, id := range []string{"ended-01", "ended-02", "ended-pending", "active-run"} {
		if strings.Contains(recent, `href="/runs/`+id+`"`) {
			t.Errorf("Recently ended includes %s", id)
		}
	}
	if !strings.Contains(dashboard, `href="/runs/ended-pending"`) {
		t.Error("a run whose cleanup is pending is not still a Needs attention item")
	}

	history := dashboardBody(t, server, "/history")
	previous := -1
	for i := 12; i >= 1; i-- {
		position := strings.Index(history, fmt.Sprintf(`href="/runs/ended-%02d"`, i))
		if position < 0 || position <= previous {
			t.Fatalf("history lacks ended-%02d or lists it out of order", i)
		}
		previous = position
	}
	for _, id := range []string{"ended-pending", "active-run"} {
		if strings.Contains(history, `href="/runs/`+id+`"`) {
			t.Errorf("history includes %s", id)
		}
	}
}
