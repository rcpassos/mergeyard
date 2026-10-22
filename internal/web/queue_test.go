package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/maintenance"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func runFor(id, repository string, number int, state workflow.State) runView {
	return runView{Run: workflow.Run{ID: id, Repository: repository, IssueNumber: number, State: state}}
}

func groupNamed(t *testing.T, groups []queueGroup, key string) queueGroup {
	t.Helper()
	for _, group := range groups {
		if group.Key == key {
			return group
		}
	}
	t.Fatalf("queue group %q is missing", key)
	return queueGroup{}
}

func hasRun(group queueGroup, id string) bool {
	for _, run := range group.Runs {
		if run.ID == id {
			return true
		}
	}
	return false
}

// Every open run and queued issue lands in exactly one documented Queue group,
// in display order, and ended runs leave the queue.
func TestQueueGroupsEachItemExactlyOnce(t *testing.T) {
	reviewPending := runFor("review-pending", "owner/repo", 13, workflow.Active)
	reviewPending.ReviewPending = true
	runs := []runView{
		runFor("attention", "owner/repo", 1, workflow.NeedsAttention),
		{Run: workflow.Run{ID: "cleanup", Repository: "owner/repo", IssueNumber: 2, State: workflow.Completed,
			Merge: &maintenance.Snapshot{ObservedAt: "2026-10-21T10:00:00Z"}}},
		// A merge is observed before the owned phase exits; pending cleanup
		// still needs attention.
		{Run: workflow.Run{ID: "early-merge", Repository: "owner/repo", IssueNumber: 14, State: workflow.Active,
			Merge: &maintenance.Snapshot{ObservedAt: "2026-10-21T10:00:00Z", Early: true}}},
		runFor("merge", "owner/repo", 3, workflow.ReadyToMerge),
		runFor("claiming", "owner/repo", 4, workflow.Claiming),
		runFor("preparing", "owner/repo", 5, workflow.Preparing),
		runFor("implement", "owner/repo", 6, workflow.Active),
		reviewPending,
		runFor("ci", "owner/repo", 7, workflow.WaitingForCI),
		runFor("manual", "owner/repo", 8, workflow.Manual),
		runFor("harness", "owner/repo", 9, workflow.WaitingForHarness),
		runFor("failed", "owner/repo", 10, workflow.Failed),
		runFor("stopped", "owner/repo", 11, workflow.Stopped),
		{Run: workflow.Run{ID: "completed", Repository: "owner/repo", IssueNumber: 12, State: workflow.Completed,
			Merge: &maintenance.Snapshot{ObservedAt: "2026-10-21T10:00:00Z", ProcessExited: true, ReadyRemoved: true,
				RunningRemoved: true, AttentionRemoved: true, IssueClosed: true, ReviewRestored: true, WorktreeRemoved: true, BranchDeleted: true}}},
	}
	issues := []scheduler.QueueIssue{
		{Repository: "owner/repo", Issue: github.Issue{Number: 20, Title: "Flagged for attention"}, Attention: true},
		{Repository: "owner/repo", Issue: github.Issue{Number: 21, Title: "Ready to claim"}},
		{Repository: "owner/repo", Issue: github.Issue{Number: 22, Title: "Waiting on another issue"}, Blockers: []github.Issue{{Number: 99, Title: "First"}}},
		{Repository: "owner/repo", Issue: github.Issue{Number: 23, Title: "Refused issue"}, Attention: true, RefusalReason: "Untrusted label actor."},
		// A run still in the queue owns its issue, including one whose merge
		// cleanup is pending, so its queue snapshot must not duplicate it.
		{Repository: "owner/repo", Issue: github.Issue{Number: 6, Title: "Already running"}},
		{Repository: "owner/repo", Issue: github.Issue{Number: 2, Title: "Cleanup still pending"}},
	}
	failures := []scheduler.Finding{{Code: "github.rate_limited", Repository: "owner/repo", Message: "GitHub rate limit reached."}}

	grouped := groupQueue(runs, issues, failures, queuePresentation{ShowRepository: true, ReadyLabels: []string{"ready-for-agent"}})

	var names []string
	for _, group := range grouped.Groups {
		names = append(names, group.Name)
	}
	wantNames := []string{"Needs attention", "Waiting for your merge", "Running", "Waiting for harness", "Ready", "Blocked"}
	if strings.Join(names, ",") != strings.Join(wantNames, ",") {
		t.Fatalf("queue groups = %v, want %v", names, wantNames)
	}

	attention := groupNamed(t, grouped.Groups, "attention")
	if !hasRun(attention, "attention") || !hasRun(attention, "cleanup") || !hasRun(attention, "early-merge") {
		t.Errorf("Needs attention runs = %+v", attention.Runs)
	}
	for _, issue := range attention.Issues {
		if issue.Issue.Number != 20 {
			t.Errorf("Needs attention holds unexpected issue #%d", issue.Issue.Number)
		}
	}
	if len(attention.Failures) != 1 {
		t.Errorf("Needs attention failures = %+v", attention.Failures)
	}

	merge := groupNamed(t, grouped.Groups, "merge")
	if len(merge.Runs) != 1 || !hasRun(merge, "merge") {
		t.Errorf("Waiting for your merge runs = %+v", merge.Runs)
	}

	running := groupNamed(t, grouped.Groups, "running")
	for _, id := range []string{"claiming", "preparing", "implement", "review-pending", "ci", "manual"} {
		if !hasRun(running, id) {
			t.Errorf("Running lacks run %q: %+v", id, running.Runs)
		}
	}
	// A draft PR waiting for review stays under Running as "Review queued".
	for _, run := range running.Runs {
		if run.ID == "review-pending" && statusOf(run.Run, run.ReviewPending).Label != "Review queued" {
			t.Errorf("draft PR label = %q, want Review queued", statusOf(run.Run, run.ReviewPending).Label)
		}
	}

	harness := groupNamed(t, grouped.Groups, "harness")
	if len(harness.Runs) != 1 || !hasRun(harness, "harness") {
		t.Errorf("Waiting for harness runs = %+v", harness.Runs)
	}

	ready := groupNamed(t, grouped.Groups, "ready")
	if len(ready.Issues) != 1 || ready.Issues[0].Issue.Number != 21 {
		t.Errorf("Ready issues = %+v", ready.Issues)
	}

	blocked := groupNamed(t, grouped.Groups, "blocked")
	if len(blocked.Issues) != 2 {
		t.Errorf("Blocked issues = %+v", blocked.Issues)
	}
	blockedNumbers := map[int]bool{}
	for _, issue := range blocked.Issues {
		blockedNumbers[issue.Issue.Number] = true
	}
	if !blockedNumbers[22] || !blockedNumbers[23] {
		t.Errorf("Blocked holds issues %v, want #22 and #23", blockedNumbers)
	}

	for _, group := range grouped.Groups {
		for _, issue := range group.Issues {
			if issue.Issue.Number == 2 {
				t.Errorf("issue #2 is duplicated beside its cleanup-pending run in %s", group.Name)
			}
		}
	}

	seen := map[string]bool{}
	total := 0
	for _, group := range grouped.Groups {
		for _, run := range group.Runs {
			if seen[run.ID] {
				t.Errorf("run %q appears more than once", run.ID)
			}
			seen[run.ID] = true
			total++
		}
	}
	if total != len(runs)-3 {
		t.Errorf("open runs in groups = %d, want %d", total, len(runs)-3)
	}
	// Ended runs leave the queue and keep the recently ended order.
	for _, run := range grouped.Recent {
		switch run.ID {
		case "failed", "stopped", "completed":
		default:
			t.Errorf("ended run %q does not belong in recent history", run.ID)
		}
	}
	if len(grouped.Recent) != 3 {
		t.Errorf("recent runs = %+v, want 3", grouped.Recent)
	}
	// Concurrency accounting is unchanged: every open run except Ready to merge.
	if grouped.Used != 9 {
		t.Errorf("concurrency slots used = %d, want 9", grouped.Used)
	}
}

// The Dashboard's Recently ended list stops at ten; the history page is the
// full view.
func TestDashboardRecentlyEndedIsBounded(t *testing.T) {
	var runs []runView
	for i := 1; i <= 12; i++ {
		runs = append(runs, runFor(fmt.Sprintf("ended-%02d", i), "owner/repo", i, workflow.Failed))
	}
	grouped := groupQueue(runs, nil, nil, queuePresentation{})
	if len(grouped.Recent) != recentEndedLimit {
		t.Fatalf("recent ended = %d runs, want %d", len(grouped.Recent), recentEndedLimit)
	}
	for i, run := range grouped.Recent {
		if run.ID != runs[i].ID {
			t.Errorf("recent ended[%d] = %s, want the caller's order %s", i, run.ID, runs[i].ID)
		}
	}
}

// The Queue renders one table with group header rows, the documented columns,
// and repository hidden when only one repository is configured.
func TestQueueTableRendersGroupsColumnsAndEmptyCopy(t *testing.T) {
	data := pageData{Page: "queue", Path: "/queue", Title: "Queue", Groups: emptyGroups()}
	data.Groups[runningIndex].Runs = []runView{runFor("run-1", "owner/repo", 7, workflow.Active)}
	body := renderContent(t, data)

	for _, text := range []string{"<table", ">Status<", ">Issue<", ">Round / attempt<", ">Updated<", ">PR<", `id="group-attention"`, `id="group-merge"`, `id="group-running"`, `id="group-harness"`, `id="group-ready"`, `id="group-blocked"`} {
		if !strings.Contains(body, text) {
			t.Errorf("queue table lacks %q", text)
		}
	}
	if strings.Contains(body, ">Repository<") {
		t.Errorf("one repository still renders the repository column: %s", body)
	}

	for index := range data.Groups {
		data.Groups[index].ShowRepository = true
	}
	body = renderContent(t, data)
	if !strings.Contains(body, ">Repository<") || !strings.Contains(body, "owner/repo") {
		t.Errorf("several repositories do not render the repository column: %s", body)
	}
}

// Each empty group explains itself; Ready names the configured label.
func TestEmptyQueueGroupsRenderTheirOwnCopy(t *testing.T) {
	body := renderContent(t, pageData{Page: "queue", Path: "/queue", Title: "Queue", Groups: emptyGroups()})
	for _, text := range []string{
		"Nothing needs you.",
		"Nothing is waiting for your merge.",
		"No runs are in progress.",
		"No runs are waiting for a harness.",
		"No eligible issues. Add the <code>ready-for-agent</code> label on GitHub.",
		"No issues are blocked.",
	} {
		if !strings.Contains(body, text) {
			t.Errorf("queue lacks empty copy %q", text)
		}
	}
}

// The Ready empty copy names the label that queues work in every configured
// repository.
func TestReadyEmptyCopyNamesConfiguredLabels(t *testing.T) {
	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/one\n    labels: {ready: ship-it}\n  - repo: owner/two\n    labels: {ready: deploy-it}\n"))
	if err != nil {
		t.Fatal(err)
	}
	presentation := queuePresentationFor(cfg)
	if len(presentation.ReadyLabels) != 2 || presentation.ReadyLabels[0] != "ship-it" || presentation.ReadyLabels[1] != "deploy-it" {
		t.Errorf("ready labels = %v, want [ship-it deploy-it]", presentation.ReadyLabels)
	}
	grouped := groupQueue(nil, nil, nil, presentation)
	body := renderContent(t, pageData{Page: "queue", Path: "/queue", Title: "Queue", Groups: grouped.Groups})
	if !strings.Contains(body, "<code>ship-it</code> or <code>deploy-it</code>") {
		t.Errorf("Ready empty copy does not name every configured label: %s", body)
	}
	if !strings.Contains(body, ">Repository<") {
		t.Errorf("two repositories do not show the repository column: %s", body)
	}
}

// A disabled repository can still own retained runs and failures, so the
// repository column follows configured repositories while Ready labels follow
// the enabled ones.
func TestQueueKeepsRepositoryIdentityForDisabledRepositories(t *testing.T) {
	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/one\n  - repo: owner/two\n    enabled: false\n    labels: {ready: disabled-label}\n"))
	if err != nil {
		t.Fatal(err)
	}
	presentation := queuePresentationFor(cfg)
	if !presentation.ShowRepository {
		t.Error("mixed enabled and disabled repositories hide the repository column")
	}
	if len(presentation.ReadyLabels) != 1 || presentation.ReadyLabels[0] != "ready-for-agent" {
		t.Errorf("Ready labels = %v, want only the enabled repository's label", presentation.ReadyLabels)
	}

	grouped := groupQueue(
		[]runView{runFor("retained", "owner/two", 7, workflow.Manual)},
		nil,
		[]scheduler.Finding{{Code: "reconcile.repository_failed", Repository: "owner/two", Message: "GitHub unavailable."}},
		presentation,
	)
	body := renderContent(t, pageData{Page: "queue", Path: "/queue", Title: "Queue", Groups: grouped.Groups})
	if !strings.Contains(body, ">Repository<") {
		t.Errorf("mixed configuration hides the repository column: %s", body)
	}
	if count := strings.Count(body, ">owner/two<"); count < 2 {
		t.Errorf("repository identity is missing from retained rows (found %d): %s", count, body)
	}
	for _, text := range []string{`href="/runs/retained"`, "GitHub failure", "GitHub unavailable."} {
		if !strings.Contains(body, text) {
			t.Errorf("retained row lacks %q: %s", text, body)
		}
	}
}

// Filter chips narrow the table to one group while leaving every chip
// available to switch groups.
func TestQueueGroupFilterNarrowsTheTable(t *testing.T) {
	data := pageData{Page: "queue", Path: "/queue", Title: "Queue", Groups: emptyGroups(), QueueFilter: "running"}
	data.Groups[runningIndex].Runs = []runView{runFor("run-1", "owner/repo", 7, workflow.Active)}
	data.Groups[blockedIndex].Issues = []issueView{{Repository: "owner/repo", Issue: github.Issue{Number: 8, Title: "Blocked title"}}}
	body := renderContent(t, data)

	if !strings.Contains(body, `id="group-running"`) || strings.Contains(body, `id="group-blocked"`) {
		t.Errorf("filter did not narrow the table: %s", body)
	}
	if !strings.Contains(body, "Blocked") || !strings.Contains(body, `href="/queue?group=blocked"`) {
		t.Errorf("filter chips lost an inactive group: %s", body)
	}
	if !strings.Contains(body, `href="/queue"`) {
		t.Errorf("filter chips lack the all-groups link: %s", body)
	}
}

// Queued issues carry the status that put them in their group.
func TestQueueIssueStatusBadges(t *testing.T) {
	issues := []issueView{
		{Repository: "owner/repo", Issue: github.Issue{Number: 1, Title: "Ready"}},
		{Repository: "owner/repo", Issue: github.Issue{Number: 2, Title: "Blocked"}, Blockers: []github.Issue{{Number: 9}}},
		{Repository: "owner/repo", Issue: github.Issue{Number: 3, Title: "Refused"}, Attention: true, RefusalReason: "Untrusted label actor."},
		{Repository: "owner/repo", Issue: github.Issue{Number: 4, Title: "Attention"}, Attention: true},
	}
	data := pageData{Page: "queue", Path: "/queue", Title: "Queue", Groups: emptyGroups()}
	data.Groups[readyIndex].Issues = issues[:1]
	data.Groups[blockedIndex].Issues = issues[1:3]
	data.Groups[attentionIndex].Issues = issues[3:]
	body := renderContent(t, data)
	for label, variant := range map[string]string{
		"Ready": "badge-success badge-outline", "Blocked": "badge-neutral", "Refused": "badge-warning", "Needs attention": "badge-error",
	} {
		if got := badgeVariant(body, label); got != variant {
			t.Errorf("issue badge %q has variant %q, want %q", label, got, variant)
		}
	}
}
