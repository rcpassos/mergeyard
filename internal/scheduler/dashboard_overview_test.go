package scheduler_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// The Dashboard is an overview: its KPIs read the same Queue groups, and Needs
// you lists exactly the Needs attention and Waiting for your merge items.
func TestDashboardOverviewKPIsNeedsYouAndQueueCountsAgree(t *testing.T) {
	t.Parallel()

	attention := testkit.Ready(202)
	attention.Labels = append(attention.Labels, github.Label{Name: "agent-needs-attention"})
	api := &testkit.FakeGitHub{
		Issues:   map[string][]github.Issue{"owner/repo": {testkit.Ready(201), attention, testkit.Ready(203)}},
		Blockers: map[int][]github.Issue{203: {testkit.Ready(99)}},
	}
	s, rt := testkit.Configured(t, api, "repositories:\n  - repo: owner/repo\n", testkit.RejectingGit{})
	testkit.SeedQueueRun(t, rt, "run-active", "owner/repo", 101, workflow.Active, workflow.Implement, 0, "2026-10-21T09:02:00Z")
	testkit.SeedQueueRun(t, rt, "run-attention", "owner/repo", 102, workflow.NeedsAttention, workflow.Fix, 0, "2026-10-21T09:03:00Z")
	testkit.SeedQueueRun(t, rt, "run-merge", "owner/repo", 103, workflow.ReadyToMerge, workflow.Review, 41, "2026-10-21T09:04:00Z")
	testkit.SeedQueueRun(t, rt, "run-harness", "owner/repo", 104, workflow.WaitingForHarness, workflow.Review, 42, "2026-10-21T09:05:00Z")
	testkit.SeedQueueRun(t, rt, "run-cleanup", "owner/repo", 105, workflow.Completed, workflow.Review, 43, "2026-10-21T09:06:00Z")
	if _, err := rt.DB.Exec(`INSERT INTO merge_cleanup(run_id,snapshot_json) VALUES(?,?)`, "run-cleanup", `{"observed_at":"2026-10-21T10:00:00Z"}`); err != nil {
		t.Fatal(err)
	}
	testkit.SeedQueueRun(t, rt, "run-ended", "owner/repo", 106, workflow.Failed, workflow.Implement, 0, "2026-10-21T08:00:00Z")

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
		queueBody = testkit.DashboardBody(t, server, "/queue")
		if strings.Contains(queueBody, "#201") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(queueBody, "#201") {
		t.Fatal("queue discovery never rendered the ready issue")
	}
	dashboard := testkit.DashboardBody(t, server, "/")

	counts := testkit.QueueGroupCounts(t, queueBody)
	kpi := testkit.Kpis(t, dashboard)
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

	needsYou := testkit.Section(t, dashboard, `id="needs-you-heading"`, `id="recently-ended-heading"`)
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
	t.Parallel()

	s, rt := testkit.Configured(t, &testkit.FakeGitHub{}, "repositories:\n  - repo: owner/repo\n", testkit.RejectingGit{})
	testkit.SeedQueueRun(t, rt, "run-active", "owner/repo", 101, workflow.Active, workflow.Implement, 0, "2026-10-21T09:02:00Z")
	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
	if err != nil {
		t.Fatal(err)
	}
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	dashboard := testkit.DashboardBody(t, server, "/")
	kpi := testkit.Kpis(t, dashboard)
	if kpi["attention"] != "0" || kpi["merge"] != "0" || kpi["running"] != "1" {
		t.Errorf("KPIs = %v, want attention 0, merge 0, running 1", kpi)
	}
	needsYou := testkit.Section(t, dashboard, `id="needs-you-heading"`, `id="recently-ended-heading"`)
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
	t.Parallel()

	s, rt := testkit.Configured(t, &testkit.FakeGitHub{}, "repositories:\n  - repo: owner/repo\n", testkit.RejectingGit{})
	for i := 1; i <= 12; i++ {
		testkit.SeedQueueRun(t, rt, fmt.Sprintf("ended-%02d", i), "owner/repo", 100+i, workflow.Failed, workflow.Implement, 0,
			fmt.Sprintf("2026-10-%02dT10:00:00Z", i))
	}
	testkit.SeedQueueRun(t, rt, "ended-pending", "owner/repo", 120, workflow.Completed, workflow.Review, 50, "2026-10-20T10:00:00Z")
	if _, err := rt.DB.Exec(`INSERT INTO merge_cleanup(run_id,snapshot_json) VALUES(?,?)`, "ended-pending", `{"observed_at":"2026-10-21T10:00:00Z"}`); err != nil {
		t.Fatal(err)
	}
	testkit.SeedQueueRun(t, rt, "active-run", "owner/repo", 121, workflow.Active, workflow.Implement, 0, "2026-10-21T10:00:00Z")
	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
	if err != nil {
		t.Fatal(err)
	}
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	dashboard := testkit.DashboardBody(t, server, "/")
	if !strings.Contains(dashboard, `href="/history"`) {
		t.Error("Recently ended has no full-history link")
	}
	recent := testkit.Section(t, dashboard, `id="recently-ended-heading"`, "</main>")
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

	history := testkit.DashboardBody(t, server, "/history")
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
