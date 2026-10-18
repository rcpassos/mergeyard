package scheduler_test

import (
	"context"
	"database/sql"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// One poll reads a repository's label-filtered listing once and shares it
// between reconcile, dispatch and the dashboard; the next poll refreshes it.
func TestPollListsLabeledIssuesOnceAndSharesTheListing(t *testing.T) {
	ctx := context.Background()
	blocked := ready(7)
	orphan := ready(8)
	orphan.Labels = []github.Label{{Name: "agent-running"}}
	attention := ready(9)
	attention.Labels = append(attention.Labels, github.Label{Name: "agent-needs-attention"})
	closed := ready(10)
	closed.State = github.Closed
	unlabeled := ready(11)
	unlabeled.Labels = nil
	api := &fakeGitHub{
		issues:   map[string][]github.Issue{"owner/repo": {blocked, orphan, attention, closed, unlabeled}},
		blockers: map[int][]github.Issue{7: {ready(99)}},
	}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s, _ := configuredAt(t, api, "repositories: [{repo: owner/repo}]", rejectingGit{}, func() time.Time { return now })

	report, err := s.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if api.calls.issueListings != 1 {
		t.Fatalf("reconcile listings = %d, want 1", api.calls.issueListings)
	}
	// Issues without a Mergeyard label are absent, but an orphaned running
	// label is still reported.
	if len(report.Findings) != 1 || report.Findings[0].Code != "reconcile.orphaned_claim" || report.Findings[0].IssueNumber != 8 {
		t.Fatalf("findings = %+v", report.Findings)
	}
	wantLabels := []string{"ready-for-agent", "agent-running", "agent-needs-attention"}
	if len(api.listings) != 1 || api.listings[0].repo != "owner/repo" || !reflect.DeepEqual(api.listings[0].labels, wantLabels) {
		t.Fatalf("listing requests = %+v", api.listings)
	}

	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if api.calls.issueListings != 2 {
		t.Fatalf("tick listings = %d, want one fresh listing for the new poll", api.calls.issueListings)
	}
	queue, err := s.Queue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The dashboard reuses the tick's listing: the blocked ready issue and the
	// attention issue, never the running, closed, or unlabelled issues.
	if len(queue) != 2 || queue[0].Issue.Number != 7 || queue[1].Issue.Number != 9 {
		t.Fatalf("queue = %+v", queue)
	}
	if api.calls.issueListings != 2 {
		t.Fatalf("queue listings = %d, want the tick's shared listing", api.calls.issueListings)
	}

	// A snapshot stays reusable for two poll intervals: one for the
	// scheduler's own refresh and one for timer skew, so the dashboard does
	// not fetch just before the scheduler refreshes the same listing.
	now = now.Add(31 * time.Second)
	if _, err := s.Queue(ctx); err != nil {
		t.Fatal(err)
	}
	if api.calls.issueListings != 2 {
		t.Fatalf("skewed queue listings = %d, want the tick's shared listing", api.calls.issueListings)
	}

	now = now.Add(30 * time.Second)
	if _, err := s.Queue(ctx); err != nil {
		t.Fatal(err)
	}
	if api.calls.issueListings != 3 {
		t.Fatalf("aged queue listings = %d, want its own refresh", api.calls.issueListings)
	}
}

// The dashboard spends a dependency read only when an issue's listing snapshot
// changes; a summary reporting no open blockers needs none at all.
func TestQueueReusesBlockerReadsForUnchangedIssues(t *testing.T) {
	ctx := context.Background()
	changedAt := time.Date(2026, 10, 18, 11, 0, 0, 0, time.UTC)
	blocked := ready(7)
	blocked.UpdatedAt = changedAt
	blocked.Dependencies = &github.DependencySummary{BlockedBy: 1}
	clear := ready(8)
	clear.UpdatedAt = changedAt
	clear.Dependencies = &github.DependencySummary{BlockedBy: 0}
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {blocked, clear}}, blockers: map[int][]github.Issue{7: {ready(99)}}}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s, _ := configuredAt(t, api, "repositories: [{repo: owner/repo}]", rejectingGit{}, func() time.Time { return now })

	queue, err := s.Queue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 2 || len(queue[0].Blockers) != 1 || len(queue[1].Blockers) != 0 {
		t.Fatalf("queue = %+v", queue)
	}
	// Only the blocked issue needed a dependency read; the unchanged
	// no-blockers summary skipped it.
	if api.calls.blockerReads != 1 {
		t.Fatalf("blocker reads = %d, want 1", api.calls.blockerReads)
	}

	// A refreshed listing with the same snapshots reuses the previous read.
	delete(api.blockers, 7)
	now = now.Add(61 * time.Second)
	queue, err = s.Queue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue[0].Blockers) != 1 || api.calls.blockerReads != 1 {
		t.Fatalf("unchanged issue re-read blockers: queue=%+v reads=%d", queue[0], api.calls.blockerReads)
	}

	// A changed issue refreshes its dependency read.
	api.issues["owner/repo"][0].UpdatedAt = changedAt.Add(time.Minute)
	api.blockers[7] = []github.Issue{ready(98), ready(99)}
	now = now.Add(61 * time.Second)
	queue, err = s.Queue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue[0].Blockers) != 2 || api.calls.blockerReads != 2 {
		t.Fatalf("changed issue kept stale blockers: queue=%+v reads=%d", queue[0], api.calls.blockerReads)
	}
}

// Refused issues lose the ready label, so the label-filtered listing no longer
// sees them; the dashboard still shows them and their reason. Refusals whose
// issue is closed or deleted are forgotten instead of re-read on every refresh.
func TestQueueKeepsRefusedIssuesVisibleWithoutLabels(t *testing.T) {
	ctx := context.Background()
	refused := ready(7)
	refused.Labels = nil
	closed := ready(8)
	closed.Labels = nil
	closed.State = github.Closed
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {refused, closed}}}
	s, runtime := configured(t, api, "repositories: [{repo: owner/repo}]", rejectingGit{})
	for _, number := range []int{7, 8, 9} {
		if _, err := runtime.DB.Exec("INSERT INTO issue_refusals(repository,issue_number,reason,ready_label,pending) VALUES('owner/repo',?,'Untrusted label actor.','ready-for-agent',0)", number); err != nil {
			t.Fatal(err)
		}
	}
	queue, err := s.Queue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// #7 is open and refused, #8 is closed, and #9 is gone.
	if len(queue) != 1 || queue[0].Issue.Number != 7 || !queue[0].Attention || queue[0].RefusalReason != "Untrusted label actor." {
		t.Fatalf("refused issue absent from dashboard: %+v", queue)
	}
	rows, err := countRefusals(runtime.DB)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("stale refusal rows = %d, want only the open refusal", rows)
	}

	// The next refresh spends one read on the visible refusal and none on the
	// forgotten rows, and reuses the listing.
	api.calls = githubCalls{}
	queue, err = s.Queue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 1 || queue[0].Issue.Number != 7 {
		t.Fatalf("queue after cleanup = %+v", queue)
	}
	if api.calls.issueReads != 1 || api.calls.issueListings != 0 {
		t.Fatalf("refresh calls = %+v, want one read and the shared listing", api.calls)
	}
}

// A pending refusal is still reconciliation's to finish: a closed issue keeps
// its row for the label removal, while a deleted issue can never complete one.
func TestQueueForgetsOnlyRefusalsNothingCanFinish(t *testing.T) {
	ctx := context.Background()
	closed := ready(11)
	closed.Labels = nil
	closed.State = github.Closed
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {closed}}}
	s, runtime := configured(t, api, "repositories: [{repo: owner/repo}]", rejectingGit{})
	for _, number := range []int{10, 11} {
		if _, err := runtime.DB.Exec("INSERT INTO issue_refusals(repository,issue_number,reason,ready_label,pending) VALUES('owner/repo',?,'Untrusted label actor.','ready-for-agent',1)", number); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Queue(ctx); err != nil {
		t.Fatal(err)
	}
	var pending []int
	rows, err := runtime.DB.Query("SELECT issue_number FROM issue_refusals ORDER BY issue_number")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var number int
		if err := rows.Scan(&number); err != nil {
			t.Fatal(err)
		}
		pending = append(pending, number)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0] != 11 {
		t.Fatalf("pending refusal rows = %v, want the closed issue's row", pending)
	}
}

// A scheduler poll and a dashboard refresh that start together share one fetch
// per repository instead of racing duplicate listings.
func TestConcurrentListingReadersShareOneFetch(t *testing.T) {
	ctx := context.Background()
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}}
	started, release := make(chan struct{}), make(chan struct{})
	var fetches int32
	api.listHook = func() {
		if atomic.AddInt32(&fetches, 1) == 1 {
			close(started)
			<-release
		}
	}
	s, _ := configured(t, api, "repositories: [{repo: owner/repo}]", rejectingGit{})

	done := make(chan error, 3)
	for range 3 {
		go func() {
			_, err := s.Queue(ctx)
			done <- err
		}()
	}
	<-started
	close(release)
	for range 3 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt32(&fetches); got != 1 {
		t.Fatalf("listing fetches = %d, want one shared fetch", got)
	}
	if api.calls.issueListings != 1 {
		t.Fatalf("listing calls = %d, want 1", api.calls.issueListings)
	}
}

func countRefusals(db *sql.DB) (int, error) {
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM issue_refusals").Scan(&count)
	return count, err
}

// A claim interrupted before saving its issue snapshot still recovers an open
// issue whose Mergeyard labels were removed: the label-filtered listing cannot
// answer whether it is open, so recovery reads the issue directly.
func TestClaimSnapshotRecoveryReadsAnOpenIssueWithoutLabels(t *testing.T) {
	ctx := context.Background()
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}}
	api.issues["owner/repo"][0].Labels = nil
	s, runtime := configured(t, api, "repositories: [{repo: owner/repo}]", rejectingGit{})
	if _, err := runtime.Workflow.Transition(ctx, "interrupted-claim", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7}); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(ctx)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
	if runs[0].LastErrorCode != "git.branch_conflict" {
		t.Fatalf("claim snapshot was not recovered: %+v", runs[0])
	}
}
