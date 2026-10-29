package scheduler_test

import (
	"context"

	"errors"
	"reflect"
	"strings"

	"testing"
	"testing/synctest"
	"time"

	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
	// One poll reads a repository's label-filtered listing once and shares it
	// between reconcile, dispatch and the dashboard; the next poll refreshes it.
)

func TestPollListsLabeledIssuesOnceAndSharesTheListing(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	blocked := testkit.Ready(7)
	orphan := testkit.Ready(8)
	orphan.Labels = []github.Label{{Name: "agent-running"}}
	attention := testkit.Ready(9)
	attention.Labels = append(attention.Labels, github.Label{Name: "agent-needs-attention"})
	closed := testkit.Ready(10)
	closed.State = github.Closed
	unlabeled := testkit.Ready(11)
	unlabeled.Labels = nil
	api := &testkit.FakeGitHub{
		Issues:   map[string][]github.Issue{"owner/repo": {blocked, orphan, attention, closed, unlabeled}},
		Blockers: map[int][]github.Issue{7: {testkit.Ready(99)}},
	}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s, _ := testkit.ConfiguredAt(t, api, "repositories: [{repo: owner/repo}]", testkit.RejectingGit{}, func() time.Time { return now })

	report, err := s.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if api.Calls.IssueListings != 1 {
		t.Fatalf("reconcile listings = %d, want 1", api.Calls.IssueListings)
	}
	// Issues without a Mergeyard label are absent, but an orphaned running
	// label is still reported.
	if len(report.Findings) != 1 || report.Findings[0].Code != "reconcile.orphaned_claim" || report.Findings[0].IssueNumber != 8 {
		t.Fatalf("findings = %+v", report.Findings)
	}
	wantLabels := []string{"ready-for-agent", "agent-running", "agent-needs-attention"}
	if len(api.Listings) != 1 || api.Listings[0].Repo != "owner/repo" || !reflect.DeepEqual(api.Listings[0].Labels, wantLabels) {
		t.Fatalf("listing requests = %+v", api.Listings)
	}

	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if api.Calls.IssueListings != 2 {
		t.Fatalf("tick listings = %d, want one fresh listing for the new poll", api.Calls.IssueListings)
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
	if api.Calls.IssueListings != 2 {
		t.Fatalf("queue listings = %d, want the tick's shared listing", api.Calls.IssueListings)
	}

	// A snapshot stays reusable for two poll intervals: one for the
	// scheduler's own refresh and one for timer skew, so the dashboard does
	// not fetch just before the scheduler refreshes the same listing.
	now = now.Add(31 * time.Second)
	if _, err := s.Queue(ctx); err != nil {
		t.Fatal(err)
	}
	if api.Calls.IssueListings != 2 {
		t.Fatalf("skewed queue listings = %d, want the tick's shared listing", api.Calls.IssueListings)
	}

	now = now.Add(30 * time.Second)
	if _, err := s.Queue(ctx); err != nil {
		t.Fatal(err)
	}
	if api.Calls.IssueListings != 3 {
		t.Fatalf("aged queue listings = %d, want its own refresh", api.Calls.IssueListings)
	}
}

// The dashboard spends a dependency read only when an issue's listing snapshot
// changes; a summary reporting no open blockers needs none at all.
func TestQueueReusesBlockerReadsForUnchangedIssues(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	changedAt := time.Date(2026, 10, 18, 11, 0, 0, 0, time.UTC)
	blocked := testkit.Ready(7)
	blocked.UpdatedAt = changedAt
	blocked.Dependencies = &github.DependencySummary{BlockedBy: 1}
	clear := testkit.Ready(8)
	clear.UpdatedAt = changedAt
	clear.Dependencies = &github.DependencySummary{BlockedBy: 0}
	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {blocked, clear}}, Blockers: map[int][]github.Issue{7: {testkit.Ready(99)}}}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s, _ := testkit.ConfiguredAt(t, api, "repositories: [{repo: owner/repo}]", testkit.RejectingGit{}, func() time.Time { return now })

	queue, err := s.Queue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 2 || len(queue[0].Blockers) != 1 || len(queue[1].Blockers) != 0 {
		t.Fatalf("queue = %+v", queue)
	}
	// Only the blocked issue needed a dependency read; the unchanged
	// no-blockers summary skipped it.
	if api.Calls.BlockerReads != 1 {
		t.Fatalf("blocker reads = %d, want 1", api.Calls.BlockerReads)
	}

	// A refreshed listing with the same snapshots reuses the previous read.
	delete(api.Blockers, 7)
	now = now.Add(61 * time.Second)
	queue, err = s.Queue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue[0].Blockers) != 1 || api.Calls.BlockerReads != 1 {
		t.Fatalf("unchanged issue re-read blockers: queue=%+v reads=%d", queue[0], api.Calls.BlockerReads)
	}

	// A changed issue refreshes its dependency read.
	api.Issues["owner/repo"][0].UpdatedAt = changedAt.Add(time.Minute)
	api.Blockers[7] = []github.Issue{testkit.Ready(98), testkit.Ready(99)}
	now = now.Add(61 * time.Second)
	queue, err = s.Queue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue[0].Blockers) != 2 || api.Calls.BlockerReads != 2 {
		t.Fatalf("changed issue kept stale blockers: queue=%+v reads=%d", queue[0], api.Calls.BlockerReads)
	}
}

// Refused issues lose the ready label, so the label-filtered listing no longer
// sees them; the dashboard still shows them and their reason. The snapshot is
// reused across refreshes, and a long TTL rereads it to forget issues that have
// closed or been deleted since.
func TestQueueCachesRefusedIssueSnapshotsAcrossRefreshes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	refused := testkit.Ready(7)
	refused.Labels = nil
	deleted := testkit.Ready(8)
	deleted.Labels = nil
	closed := testkit.Ready(9)
	closed.Labels = nil
	closed.State = github.Closed
	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {refused, deleted, closed}}}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s, runtime := testkit.ConfiguredAt(t, api, "repositories: [{repo: owner/repo}]", testkit.RejectingGit{}, func() time.Time { return now })
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
	rows, err := testkit.CountRefusals(runtime.DB)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("stale refusal rows = %d, want only the open refusals", rows)
	}
	if api.Calls.IssueReads != 4 {
		t.Fatalf("initial refused-issue reads = %d, want one per refusal", api.Calls.IssueReads)
	}

	// Several refreshes reuse the open issues' snapshots without reading them
	// again, even as the label-filtered listing itself expires and refreshes.
	api.Calls = testkit.GithubCalls{}
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
	if api.Calls.IssueReads != 0 {
		t.Fatalf("steady-state refused-issue reads = %d, want none", api.Calls.IssueReads)
	}

	// Once the TTL expires, the cached issues are read again: the now-closed
	// and deleted ones are forgotten instead of staying visible forever.
	closedAfter := testkit.Ready(7)
	closedAfter.Labels = nil
	closedAfter.State = github.Closed
	api.Issues["owner/repo"] = []github.Issue{closedAfter}
	now = now.Add(16 * time.Minute)
	queue, err = s.Queue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 0 {
		t.Fatalf("closed or deleted refusals stayed visible: %+v", queue)
	}
	if rows, err = testkit.CountRefusals(runtime.DB); err != nil || rows != 0 {
		t.Fatalf("stale refusal rows = %d, want none: %v", rows, err)
	}
	if api.Calls.IssueReads != 2 {
		t.Fatalf("expired refused-issue reads = %d, want one per open refusal", api.Calls.IssueReads)
	}
}

// A failed refusal reread keeps the last snapshot and backs off instead of
// failing the queue or spending another call on the next refresh.
func TestQueueKeepsLastRefusedSnapshotWhenRereadFails(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	refused := testkit.Ready(7)
	refused.Labels = nil
	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {refused, testkit.Ready(8)}}}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s, runtime := testkit.ConfiguredAt(t, api, "repositories: [{repo: owner/repo}]", testkit.RejectingGit{}, func() time.Time { return now })
	if _, err := runtime.DB.Exec("INSERT INTO issue_refusals(repository,issue_number,reason,ready_label,pending) VALUES('owner/repo',7,'Untrusted label actor.','ready-for-agent',0)"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Queue(ctx); err != nil {
		t.Fatal(err)
	}
	api.Calls = testkit.GithubCalls{}
	now = now.Add(30 * time.Second)
	if _, err := s.Queue(ctx); err != nil {
		t.Fatal(err)
	}
	if api.Calls.IssueReads != 0 {
		t.Fatalf("cached refusal read = %d, want none", api.Calls.IssueReads)
	}

	now = now.Add(16 * time.Minute)
	api.GetIssueHook = func(string, int) (github.Issue, error) {
		return github.Issue{}, &fault.Error{Code: "github.rate_limited", Message: "API rate limit exceeded"}
	}
	queue, err := s.Queue(ctx)
	if err != nil {
		t.Fatalf("failed refusal reread failed the queue: %v", err)
	}
	if len(queue) != 2 || queue[0].Issue.Number != 7 || queue[0].RefusalReason != "Untrusted label actor." {
		t.Fatalf("failed reread lost the refused snapshot: %+v", queue)
	}
	if api.Calls.IssueReads != 1 {
		t.Fatalf("expired refusal reads = %d, want 1", api.Calls.IssueReads)
	}

	// The failed read backs off for a full window rather than retrying on
	// every dashboard refresh.
	api.Calls = testkit.GithubCalls{}
	now = now.Add(30 * time.Second)
	if _, err := s.Queue(ctx); err != nil {
		t.Fatal(err)
	}
	if api.Calls.IssueReads != 0 {
		t.Fatalf("failed refusal reread retried immediately: %d reads", api.Calls.IssueReads)
	}
}

// A refused issue that cannot be read at all is scoped to that issue: the rest
// of the repository and every other repository stay visible.
func TestQueueScopesUnreadableRefusedIssues(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	broken := testkit.Ready(1)
	broken.Labels = nil
	healthy := testkit.Ready(1)
	healthy.Labels = nil
	healthy.Title = "Healthy refusal"
	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{
		"owner/broken": {broken},
		"owner/ok":     {healthy},
	}}
	api.GetIssueHook = func(repo string, _ int) (github.Issue, error) {
		if repo == "owner/broken" {
			return github.Issue{}, &fault.Error{Code: "github.unavailable", Message: "dial tcp: connection refused"}
		}
		return healthy, nil
	}
	s, runtime := testkit.Configured(t, api, "repositories:\n  - repo: owner/broken\n  - repo: owner/ok\n", testkit.RejectingGit{})
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
	api.Calls = testkit.GithubCalls{}
	report, err = s.QueueReport(ctx)
	if !scheduler.GitHubFailure(err) {
		t.Fatalf("queue error = %v, want the remembered refused-issue failure", err)
	}
	if api.Calls.IssueReads != 0 {
		t.Fatalf("failed refused issue was retried on the next refresh: %d reads", api.Calls.IssueReads)
	}
	if len(report.Failures) != 1 || report.Failures[0].IssueNumber != 1 {
		t.Fatalf("queue failures = %+v", report.Failures)
	}
}

// A refused-issue read that reaches its own full window stays scoped to its
// issue instead of discarding discoveries from the rest of the pass, and backs
// off so the next refresh does not return to the same slow read.
func TestQueueScopesRefusedIssueDeadlineAndBacksOff(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	for _, tc := range []struct {
		name, repositories string
	}{
		{"healthy repository first", "repositories:\n  - repo: owner/healthy\n  - repo: owner/broken\n"},
		{"broken repository first", "repositories:\n  - repo: owner/broken\n  - repo: owner/healthy\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {

				api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{
					"owner/healthy": {testkit.Ready(1)},
					"owner/broken":  {testkit.Ready(8)},
				}}
				gated := &testkit.DeadlineGitHub{FakeGitHub: api, SlowRepo: "owner/broken"}
				s, runtime := testkit.Configured(t, gated, tc.repositories, testkit.RejectingGit{})
				if _, err := runtime.DB.Exec("INSERT INTO issue_refusals(repository,issue_number,reason,ready_label,pending) VALUES('owner/broken',7,'Untrusted label actor.','ready-for-agent',0)"); err != nil {
					t.Fatal(err)
				}
				for pass := 1; pass <= 2; pass++ {
					// The window fits in the refresh budget, so the read reaches
					// GitHub's own timeout and the other repository still fits.
					refreshCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
					report, err := s.QueueReport(refreshCtx)
					cancel()
					if err == nil || !scheduler.GitHubFailure(err) {
						t.Fatalf("pass %d queue error = %v, want a scoped refused-issue timeout", pass, err)
					}
					var healthy, beside bool
					for _, item := range report.Issues {
						if strings.EqualFold(item.Repository, "owner/healthy") && item.Issue.Number == 1 {
							healthy = true
						}
						if strings.EqualFold(item.Repository, "owner/broken") && item.Issue.Number == 8 {
							beside = true
						}
					}
					if !healthy || !beside || len(report.Issues) != 2 {
						t.Fatalf("pass %d lost healthy issues: %+v", pass, report.Issues)
					}
					if len(report.Failures) != 1 || report.Failures[0].Code != "reconcile.issue_failed" || report.Failures[0].IssueNumber != 7 || !strings.Contains(report.Failures[0].Message, "github.timeout") {
						t.Fatalf("pass %d queue failures = %+v", pass, report.Failures)
					}
				}
				if api.Calls.IssueReads != 1 {
					t.Fatalf("refused-issue reads across deadline passes = %d, want one before backoff", api.Calls.IssueReads)
				}

			})
		})
	}
}

// A shutdown or disconnected caller aborts the pass without recording the
// refused read as the issue's cached state, so the next refresh retries it.
func TestQueueDoesNotCacheRefusedReadCancellation(t *testing.T) {
	t.Parallel()

	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{
		"owner/healthy": {testkit.Ready(1)},
		"owner/broken":  {},
	}}
	started := make(chan struct{})
	gated := &testkit.DeadlineGitHub{FakeGitHub: api, SlowRepo: "owner/broken", Started: started}
	s, runtime := testkit.Configured(t, gated, "repositories:\n  - repo: owner/healthy\n  - repo: owner/broken\n", testkit.RejectingGit{})
	if _, err := runtime.DB.Exec("INSERT INTO issue_refusals(repository,issue_number,reason,ready_label,pending) VALUES('owner/broken',7,'Untrusted label actor.','ready-for-agent',0)"); err != nil {
		t.Fatal(err)
	}
	refreshCtx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	report, err := s.QueueReport(refreshCtx)
	if err == nil || scheduler.GitHubFailure(err) || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled queue error = %v, want the caller's cancellation", err)
	}
	if len(report.Issues) != 1 || report.Issues[0].Issue.Number != 1 {
		t.Fatalf("canceled pass lost the healthy repository: %+v", report.Issues)
	}

	// The canceled read was not cached: the next pass tries it again.
	api.Calls = testkit.GithubCalls{}
	gated.SlowRepo = ""
	deadlineCtx, deadlineCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer deadlineCancel()
	if _, err := s.QueueReport(deadlineCtx); err != nil {
		t.Fatalf("retried queue error = %v, want the retried read to succeed", err)
	}
	if api.Calls.IssueReads != 1 {
		t.Fatalf("canceled refused read was cached: %d reads on retry", api.Calls.IssueReads)
	}
}

// A trusted relabel returns the issue to the label-filtered listing, so the
// dashboard uses that current snapshot instead of the cached label-less one.
func TestQueueUsesRelabelledIssueFromListing(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	refused := testkit.Ready(7)
	refused.Labels = nil
	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {refused}}}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s, runtime := testkit.ConfiguredAt(t, api, "repositories: [{repo: owner/repo}]", testkit.RejectingGit{}, func() time.Time { return now })
	if _, err := runtime.DB.Exec("INSERT INTO issue_refusals(repository,issue_number,reason,ready_label,pending) VALUES('owner/repo',7,'Untrusted label actor.','ready-for-agent',0)"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Queue(ctx); err != nil {
		t.Fatal(err)
	}

	api.Issues["owner/repo"][0].Labels = []github.Label{{Name: "ready-for-agent"}}
	api.Calls = testkit.GithubCalls{}
	now = now.Add(61 * time.Second)
	queue, err := s.Queue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 1 || !testkit.Has(queue[0].Issue, "ready-for-agent") || queue[0].RefusalReason != "Untrusted label actor." {
		t.Fatalf("relabelled issue did not use the listing: %+v", queue)
	}
	if api.Calls.IssueReads != 0 {
		t.Fatalf("relabelled issue spent a refused-issue read: %d", api.Calls.IssueReads)
	}

	// If the label disappears again, the stale snapshot is gone and the issue
	// is read back once.
	api.Issues["owner/repo"][0].Labels = nil
	api.Calls = testkit.GithubCalls{}
	now = now.Add(61 * time.Second)
	queue, err = s.Queue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 1 || queue[0].Issue.Number != 7 || api.Calls.IssueReads != 1 {
		t.Fatalf("relabelled issue snapshot was reused after the label was removed: %+v reads=%d", queue, api.Calls.IssueReads)
	}
}

// A pending refusal is still reconciliation's to finish: a closed issue keeps
// its row for the label removal, while a deleted issue can never complete one.
func TestQueueForgetsOnlyRefusalsNothingCanFinish(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	closed := testkit.Ready(11)
	closed.Labels = nil
	closed.State = github.Closed
	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {closed}}}
	s, runtime := testkit.Configured(t, api, "repositories: [{repo: owner/repo}]", testkit.RejectingGit{})
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
	api.Calls = testkit.GithubCalls{}
	if _, err := s.Queue(ctx); err != nil {
		t.Fatal(err)
	}
	if api.Calls.IssueReads != 0 {
		t.Fatalf("pending closed refusal was re-read: %d reads", api.Calls.IssueReads)
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

// A caller waiting behind a stalled listing fetch returns on its own deadline
// instead of waiting for the fetch to finish.
func TestListingWaiterReturnsWhenItsDeadlineExpires(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {

		api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}}
		started, unblock := testkit.StallListing(api)
		defer unblock()
		s, _ := testkit.Configured(t, api, "repositories: [{repo: owner/repo}]", testkit.RejectingGit{})

		firstDone := make(chan error, 1)
		go func() {
			_, err := s.Queue(context.Background())
			firstDone <- err
		}()
		<-started

		waitCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := s.Queue(waitCtx)
			done <- err
		}()
		start := time.Now()
		select {
		case err := <-done:
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Fatalf("waiter returned after %s", elapsed)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("waiter error = %v, want its deadline", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("waiter ignored its deadline while the listing fetch was stalled")
		}

		unblock()
		if err := <-firstDone; err != nil {
			t.Fatal(err)
		}

	})
}

// A dashboard refresh that runs out of budget behind a stalled listing fetch
// returns the last good queue instead of failing discovery.
func TestQueueReportsLastGoodListingWhenRefreshDeadlineExpires(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {

		now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
		api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}}
		s, _ := testkit.ConfiguredAt(t, api, "repositories: [{repo: owner/repo}]", testkit.RejectingGit{}, func() time.Time { return now })
		if _, err := s.Queue(context.Background()); err != nil {
			t.Fatal(err)
		}

		// Age the shared listing so the next reader must fetch again, then stall
		// that fetch while a second reader waits.
		now = now.Add(61 * time.Second)
		started, unblock := testkit.StallListing(api)
		defer unblock()
		firstDone := make(chan error, 1)
		go func() {
			_, err := s.Queue(context.Background())
			firstDone <- err
		}()
		<-started

		type result struct {
			report scheduler.QueueSnapshot
			Err    error
		}
		refreshCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		done := make(chan result, 1)
		go func() {
			report, err := s.QueueReport(refreshCtx)
			done <- result{report: report, Err: err}
		}()
		start := time.Now()
		select {
		case got := <-done:
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Fatalf("refresh returned after %s", elapsed)
			}
			if got.Err != nil {
				t.Fatalf("refresh error = %v, want the last good listing", got.Err)
			}
			if !got.report.Stale {
				t.Fatal("expired refresh did not report a stale snapshot")
			}
			if len(got.report.Issues) != 1 || got.report.Issues[0].Issue.Number != 7 {
				t.Fatalf("refresh lost the last good listing: %+v", got.report.Issues)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("refresh ignored its deadline while the listing fetch was stalled")
		}

		unblock()
		if err := <-firstDone; err != nil {
			t.Fatal(err)
		}

	})
}

// A scheduler poll and a dashboard refresh that start together share one fetch
// per repository instead of racing duplicate listings.
func TestConcurrentListingReadersShareOneFetch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}}
	started, unblock := testkit.StallListing(api)
	defer unblock()
	s, _ := testkit.Configured(t, api, "repositories: [{repo: owner/repo}]", testkit.RejectingGit{})

	done := make(chan error, 3)
	for range 3 {
		go func() {
			_, err := s.Queue(ctx)
			done <- err
		}()
	}
	<-started
	unblock()
	for range 3 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if api.Calls.IssueListings != 1 {
		t.Fatalf("listing calls = %d, want 1", api.Calls.IssueListings)
	}
}

// A claim interrupted before saving its issue snapshot still recovers an open
// issue whose Mergeyard labels were removed: the label-filtered listing cannot
// answer whether it is open, so recovery reads the issue directly.
func TestClaimSnapshotRecoveryReadsAnOpenIssueWithoutLabels(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}}
	api.Issues["owner/repo"][0].Labels = nil
	s, runtime := testkit.Configured(t, api, "repositories: [{repo: owner/repo}]", testkit.RejectingGit{})
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
