package scheduler_test

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
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
// sees them; the dashboard still shows them and their reason. The snapshot is
// reused across refreshes, and a long TTL rereads it to forget issues that have
// closed or been deleted since.
func TestQueueCachesRefusedIssueSnapshotsAcrossRefreshes(t *testing.T) {
	ctx := context.Background()
	refused := ready(7)
	refused.Labels = nil
	deleted := ready(8)
	deleted.Labels = nil
	closed := ready(9)
	closed.Labels = nil
	closed.State = github.Closed
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {refused, deleted, closed}}}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s, runtime := configuredAt(t, api, "repositories: [{repo: owner/repo}]", rejectingGit{}, func() time.Time { return now })
	for _, number := range []int{7, 8, 9, 10} {
		if _, err := runtime.DB.Exec("INSERT INTO issue_refusals(repository,issue_number,reason,ready_label,pending) VALUES('owner/repo',?,'Untrusted label actor.','ready-for-agent',0)", number); err != nil {
			t.Fatal(err)
		}
	}
	queue, err := s.Queue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// #7 and #8 are open and refused; #9 is closed, and #10 is gone.
	if len(queue) != 2 || queue[0].Issue.Number != 7 || queue[1].Issue.Number != 8 || !queue[0].Attention || queue[0].RefusalReason != "Untrusted label actor." {
		t.Fatalf("refused issues absent from dashboard: %+v", queue)
	}
	rows, err := countRefusals(runtime.DB)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("stale refusal rows = %d, want only the open refusals", rows)
	}
	if api.calls.issueReads != 4 {
		t.Fatalf("initial refused-issue reads = %d, want one per refusal", api.calls.issueReads)
	}

	// Several refreshes reuse the open issues' snapshots without reading them
	// again, even as the label-filtered listing itself expires and refreshes.
	api.calls = githubCalls{}
	for range 3 {
		now = now.Add(30 * time.Second)
		queue, err = s.Queue(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(queue) != 2 || queue[0].Issue.Number != 7 || queue[1].Issue.Number != 8 {
			t.Fatalf("refused issues disappeared on refresh: %+v", queue)
		}
	}
	if api.calls.issueReads != 0 {
		t.Fatalf("steady-state refused-issue reads = %d, want none", api.calls.issueReads)
	}

	// Once the TTL expires, the cached issues are read again: the now-closed
	// and deleted ones are forgotten instead of staying visible forever.
	closedAfter := ready(7)
	closedAfter.Labels = nil
	closedAfter.State = github.Closed
	api.issues["owner/repo"] = []github.Issue{closedAfter}
	now = now.Add(16 * time.Minute)
	queue, err = s.Queue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 0 {
		t.Fatalf("closed or deleted refusals stayed visible: %+v", queue)
	}
	if rows, err = countRefusals(runtime.DB); err != nil || rows != 0 {
		t.Fatalf("stale refusal rows = %d, want none: %v", rows, err)
	}
	if api.calls.issueReads != 2 {
		t.Fatalf("expired refused-issue reads = %d, want one per open refusal", api.calls.issueReads)
	}
}

// A failed refusal reread keeps the last snapshot and backs off instead of
// failing the queue or spending another call on the next refresh.
func TestQueueKeepsLastRefusedSnapshotWhenRereadFails(t *testing.T) {
	ctx := context.Background()
	refused := ready(7)
	refused.Labels = nil
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {refused, ready(8)}}}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s, runtime := configuredAt(t, api, "repositories: [{repo: owner/repo}]", rejectingGit{}, func() time.Time { return now })
	if _, err := runtime.DB.Exec("INSERT INTO issue_refusals(repository,issue_number,reason,ready_label,pending) VALUES('owner/repo',7,'Untrusted label actor.','ready-for-agent',0)"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Queue(ctx); err != nil {
		t.Fatal(err)
	}
	api.calls = githubCalls{}
	now = now.Add(30 * time.Second)
	if _, err := s.Queue(ctx); err != nil {
		t.Fatal(err)
	}
	if api.calls.issueReads != 0 {
		t.Fatalf("cached refusal read = %d, want none", api.calls.issueReads)
	}

	now = now.Add(16 * time.Minute)
	api.getIssue = func(string, int) (github.Issue, error) {
		return github.Issue{}, &fault.Error{Code: "github.rate_limited", Message: "API rate limit exceeded"}
	}
	queue, err := s.Queue(ctx)
	if err != nil {
		t.Fatalf("failed refusal reread failed the queue: %v", err)
	}
	if len(queue) != 2 || queue[0].Issue.Number != 7 || queue[0].RefusalReason != "Untrusted label actor." {
		t.Fatalf("failed reread lost the refused snapshot: %+v", queue)
	}
	if api.calls.issueReads != 1 {
		t.Fatalf("expired refusal reads = %d, want 1", api.calls.issueReads)
	}

	// The failed read backs off for a full window rather than retrying on
	// every dashboard refresh.
	api.calls = githubCalls{}
	now = now.Add(30 * time.Second)
	if _, err := s.Queue(ctx); err != nil {
		t.Fatal(err)
	}
	if api.calls.issueReads != 0 {
		t.Fatalf("failed refusal reread retried immediately: %d reads", api.calls.issueReads)
	}
}

// A refused issue that cannot be read at all is scoped to that issue: the rest
// of the repository and every other repository stay visible.
func TestQueueScopesUnreadableRefusedIssues(t *testing.T) {
	ctx := context.Background()
	broken := ready(1)
	broken.Labels = nil
	healthy := ready(1)
	healthy.Labels = nil
	healthy.Title = "Healthy refusal"
	api := &fakeGitHub{issues: map[string][]github.Issue{
		"owner/broken": {broken},
		"owner/ok":     {healthy},
	}}
	api.getIssue = func(repo string, _ int) (github.Issue, error) {
		if repo == "owner/broken" {
			return github.Issue{}, &fault.Error{Code: "github.unavailable", Message: "dial tcp: connection refused"}
		}
		return healthy, nil
	}
	s, runtime := configured(t, api, "repositories:\n  - repo: owner/broken\n  - repo: owner/ok\n", rejectingGit{})
	for _, repo := range []string{"owner/broken", "owner/ok"} {
		if _, err := runtime.DB.Exec("INSERT INTO issue_refusals(repository,issue_number,reason,ready_label,pending) VALUES(?,1,'Untrusted label actor.','ready-for-agent',0)", repo); err != nil {
			t.Fatal(err)
		}
	}
	report, err := s.QueueReport(ctx)
	if !scheduler.GitHubFailure(err) {
		t.Fatalf("queue error = %v, want a scoped refused-issue failure", err)
	}
	if len(report.Issues) != 1 || !strings.EqualFold(report.Issues[0].Repository, "owner/ok") || report.Issues[0].Issue.Number != 1 || report.Issues[0].RefusalReason == "" {
		t.Fatalf("readable refusal disappeared from the queue: %+v", report.Issues)
	}
	if len(report.Failures) != 1 || report.Failures[0].Code != "reconcile.issue_failed" || report.Failures[0].IssueNumber != 1 || !strings.EqualFold(report.Failures[0].Repository, "owner/broken") {
		t.Fatalf("queue failures = %+v", report.Failures)
	}

	// A failed first read is remembered for the window: the next refresh keeps
	// reporting the issue's failure without spending another call.
	api.calls = githubCalls{}
	report, err = s.QueueReport(ctx)
	if !scheduler.GitHubFailure(err) {
		t.Fatalf("queue error = %v, want the remembered refused-issue failure", err)
	}
	if api.calls.issueReads != 0 {
		t.Fatalf("failed refused issue was retried on the next refresh: %d reads", api.calls.issueReads)
	}
	if len(report.Failures) != 1 || report.Failures[0].IssueNumber != 1 {
		t.Fatalf("queue failures = %+v", report.Failures)
	}
}

// A trusted relabel returns the issue to the label-filtered listing, so the
// dashboard uses that current snapshot instead of the cached label-less one.
func TestQueueUsesRelabelledIssueFromListing(t *testing.T) {
	ctx := context.Background()
	refused := ready(7)
	refused.Labels = nil
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {refused}}}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s, runtime := configuredAt(t, api, "repositories: [{repo: owner/repo}]", rejectingGit{}, func() time.Time { return now })
	if _, err := runtime.DB.Exec("INSERT INTO issue_refusals(repository,issue_number,reason,ready_label,pending) VALUES('owner/repo',7,'Untrusted label actor.','ready-for-agent',0)"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Queue(ctx); err != nil {
		t.Fatal(err)
	}

	api.issues["owner/repo"][0].Labels = []github.Label{{Name: "ready-for-agent"}}
	api.calls = githubCalls{}
	now = now.Add(61 * time.Second)
	queue, err := s.Queue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 1 || !has(queue[0].Issue, "ready-for-agent") || queue[0].RefusalReason != "Untrusted label actor." {
		t.Fatalf("relabelled issue did not use the listing: %+v", queue)
	}
	if api.calls.issueReads != 0 {
		t.Fatalf("relabelled issue spent a refused-issue read: %d", api.calls.issueReads)
	}

	// If the label disappears again, the stale snapshot is gone and the issue
	// is read back once.
	api.issues["owner/repo"][0].Labels = nil
	api.calls = githubCalls{}
	now = now.Add(61 * time.Second)
	queue, err = s.Queue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 1 || queue[0].Issue.Number != 7 || api.calls.issueReads != 1 {
		t.Fatalf("relabelled issue snapshot was reused after the label was removed: %+v reads=%d", queue, api.calls.issueReads)
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
	// The closed pending issue keeps its snapshot because reconciliation still
	// owns its row: the next refresh must not read it again.
	api.calls = githubCalls{}
	if _, err := s.Queue(ctx); err != nil {
		t.Fatal(err)
	}
	if api.calls.issueReads != 0 {
		t.Fatalf("pending closed refusal was re-read: %d reads", api.calls.issueReads)
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
