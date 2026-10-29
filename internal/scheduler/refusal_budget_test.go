package scheduler_test

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
)

// Thirty refused issues that expire together must not spend the whole
// refresh. A refresh too short to give a read its full window starts none of
// them, and the pass still returns every repository's queue from the last
// readable snapshots.
func TestQueueKeepsEveryRepositoryWhenRefusedIssuesExpireTogether(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {

		const refusals = 15
		api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{}}
		slow := &testkit.SlowGitHub{FakeGitHub: api, Delay: 30 * time.Millisecond}
		now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
		s, runtime := testkit.ConfiguredAt(t, slow, "repositories:\n  - repo: owner/first\n  - repo: owner/second\n", testkit.RejectingGit{}, func() time.Time { return now })
		testkit.RefuseIssues(t, runtime, api, []string{"owner/first", "owner/second"}, refusals)

		// The first pass reads every refused issue back and caches its snapshot.
		first, cancelFirst := context.WithTimeout(context.Background(), 30*time.Second)
		report, err := s.QueueReport(first)
		cancelFirst()
		if err != nil {
			t.Fatalf("first queue report = %v, want a complete queue", err)
		}
		if len(report.Issues) != 2*refusals+2 {
			t.Fatalf("first queue = %d issues, want %d", len(report.Issues), 2*refusals+2)
		}
		if api.Calls.IssueReads != 2*refusals {
			t.Fatalf("first refused-issue reads = %d, want %d", api.Calls.IssueReads, 2*refusals)
		}

		// The snapshots expire together, but the next refresh is too short to
		// give a refused read its full window, so it keeps every snapshot.
		now = now.Add(16 * time.Minute)
		api.Calls = testkit.GithubCalls{}
		second, cancelSecond := context.WithTimeout(context.Background(), 600*time.Millisecond)
		report, err = s.QueueReport(second)
		cancelSecond()
		if err != nil {
			t.Fatalf("second queue report = %v, want a partial queue instead of a failed one", err)
		}
		if len(report.Issues) != 2*refusals+2 {
			t.Fatalf("second queue = %d issues, want %d: the refresh dropped the queue", len(report.Issues), 2*refusals+2)
		}
		if api.Calls.IssueReads != 0 {
			t.Fatalf("refused-issue reads = %d, want no read the refresh cannot afford", api.Calls.IssueReads)
		}
		seen := map[string]int{}
		for _, item := range report.Issues {
			seen[strings.ToLower(item.Repository)]++
			if item.Issue.Number <= refusals && item.RefusalReason == "" {
				t.Fatalf("refused issue %d lost its reason: %+v", item.Issue.Number, item)
			}
		}
		if len(seen) != 2 {
			t.Fatalf("the refresh lost a repository: %+v", seen)
		}

	})
}

// A refresh that cannot give a refused read its full window does not start it.
// The refusal keeps its last readable snapshot, and the next refresh reads it
// instead of backing off for a whole window.
func TestQueueKeepsSnapshotWhenTheRefreshCannotAffordARead(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {

		api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{}}
		now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
		s, runtime := testkit.ConfiguredAt(t, api, "repositories: [{repo: owner/first}]", testkit.RejectingGit{}, func() time.Time { return now })
		testkit.RefuseIssues(t, runtime, api, []string{"owner/first"}, 2)

		first, cancelFirst := context.WithTimeout(context.Background(), 30*time.Second)
		report, err := s.QueueReport(first)
		cancelFirst()
		if err != nil || len(report.Issues) != 3 || api.Calls.IssueReads != 2 {
			t.Fatalf("first queue = %d issues, reads = %d, err = %v; want both refusals read", len(report.Issues), api.Calls.IssueReads, err)
		}

		// Both snapshots expire together, but the refresh cannot afford a read
		// with a full window, so neither is started.
		now = now.Add(16 * time.Minute)
		api.Calls = testkit.GithubCalls{}
		second, cancelSecond := context.WithTimeout(context.Background(), 500*time.Millisecond)
		report, err = s.QueueReport(second)
		cancelSecond()
		if err != nil {
			t.Fatalf("second queue error = %v, want the snapshots kept", err)
		}
		if api.Calls.IssueReads != 0 {
			t.Fatalf("refused-issue reads = %d, want no read the refresh cannot afford", api.Calls.IssueReads)
		}
		if len(report.Issues) != 3 {
			t.Fatalf("queue = %d issues, want the unread refusals' snapshots kept: %+v", len(report.Issues), report.Issues)
		}
		for _, item := range report.Issues {
			if item.Issue.Number <= 2 && item.RefusalReason == "" {
				t.Fatalf("refused issue %d lost its snapshot or reason: %+v", item.Issue.Number, item)
			}
		}

		// The next refresh can afford the reads again, so the refusals are read
		// rather than hidden for the whole window.
		now = now.Add(31 * time.Second)
		api.Calls = testkit.GithubCalls{}
		report, err = s.QueueReport(context.Background())
		if err != nil || api.Calls.IssueReads != 2 || len(report.Issues) != 3 {
			t.Fatalf("retry queue = %d issues, reads = %d, err = %v; want the kept refusals read", len(report.Issues), api.Calls.IssueReads, err)
		}

	})
}

// A refresh that cannot afford refused reads still assembles the queue from
// their snapshots: the local lookups do not share the budget reserved for
// GitHub.
func TestQueueKeepsRefusalReasonsWhenTheRefreshCannotAffordReads(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {

		const refusals = 15
		api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{}}
		now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
		s, runtime := testkit.ConfiguredAt(t, api, "repositories: [{repo: owner/first}]", testkit.RejectingGit{}, func() time.Time { return now })
		testkit.RefuseIssues(t, runtime, api, []string{"owner/first"}, refusals)

		// The first pass caches the snapshots of every refused issue.
		first, cancelFirst := context.WithTimeout(context.Background(), 30*time.Second)
		report, err := s.QueueReport(first)
		cancelFirst()
		if err != nil {
			t.Fatalf("first queue report = %v, want a complete queue", err)
		}
		if len(report.Issues) != refusals+1 {
			t.Fatalf("first queue = %d issues, want %d", len(report.Issues), refusals+1)
		}

		// The snapshots expire together, but the next refresh is too short to
		// give a refused read its full window, so it keeps every snapshot.
		now = now.Add(16 * time.Minute)
		second, cancelSecond := context.WithTimeout(context.Background(), 300*time.Millisecond)
		report, err = s.QueueReport(second)
		cancelSecond()
		if err != nil {
			t.Fatalf("queue report = %v, want the queue to survive the spent budget", err)
		}
		if len(report.Issues) != refusals+1 {
			t.Fatalf("queue = %d issues, want %d", len(report.Issues), refusals+1)
		}
		for _, item := range report.Issues {
			if item.Issue.Number <= refusals && item.RefusalReason == "" {
				t.Fatalf("refused issue %d lost its reason after the budget was spent: %+v", item.Issue.Number, item)
			}
		}

	})
}

// A slow refused read that fits its window stays scoped to its issue. The
// repositories after it are still listed instead of losing their queue to a
// spent GitHub budget.
func TestQueueKeepsDiscoveringAfterASlowRefusedRead(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {

		api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{}}
		gated := &testkit.DeadlineGitHub{FakeGitHub: api, SlowRepo: "owner/first"}
		now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
		s, runtime := testkit.ConfiguredAt(t, gated, "repositories:\n  - repo: owner/first\n  - repo: owner/second\n", testkit.RejectingGit{}, func() time.Time { return now })
		testkit.RefuseIssues(t, runtime, api, []string{"owner/first", "owner/second"}, 3)

		refresh, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		report, err := s.QueueReport(refresh)
		cancel()
		if !scheduler.GitHubFailure(err) {
			t.Fatalf("queue error = %v, want a scoped refused-issue failure instead of a failed queue", err)
		}
		if len(report.Failures) != 1 || report.Failures[0].IssueNumber != 1 || !strings.EqualFold(report.Failures[0].Repository, "owner/first") {
			t.Fatalf("queue failures = %+v, want only the slow refusal", report.Failures)
		}
		var first, second bool
		for _, item := range report.Issues {
			if item.Issue.Number != 100 && item.Issue.Number != 101 {
				continue
			}
			if strings.EqualFold(item.Repository, "owner/first") {
				first = true
			}
			if strings.EqualFold(item.Repository, "owner/second") {
				second = true
			}
		}
		if !first || !second {
			t.Fatalf("a repository lost its queue after the slow read: %+v", report.Issues)
		}

	})
}

// A read that blocks until its own window ends is a genuine GitHub hang, like
// the real adapter reports. It backs off for the full window, so an outage
// does not spend one call per refused issue on every refresh.
func TestQueueBacksOffTheWholeWindowAfterAGenuineTimeout(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {

		api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{}}
		now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
		gated := &testkit.DeadlineGitHub{FakeGitHub: api, SlowRepo: "owner/first"}
		s, runtime := testkit.ConfiguredAt(t, gated, "repositories: [{repo: owner/first}]", testkit.RejectingGit{}, func() time.Time { return now })
		testkit.RefuseIssues(t, runtime, api, []string{"owner/first"}, 1)

		// The read reaches its own full window before the refresh budget ends,
		// so the timeout belongs to GitHub and not to the budget.
		refresh, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		report, err := s.QueueReport(refresh)
		cancel()
		if err == nil || !scheduler.GitHubFailure(err) {
			t.Fatalf("queue error = %v, want a scoped refused-issue timeout", err)
		}
		if api.Calls.IssueReads != 1 || len(report.Failures) != 1 || report.Failures[0].IssueNumber != 1 {
			t.Fatalf("reads = %d, queue failures = %+v, want one timeout", api.Calls.IssueReads, report.Failures)
		}

		// The failure backs off for the whole window instead of every refresh.
		now = now.Add(31 * time.Second)
		gated.SlowRepo = ""
		api.Calls = testkit.GithubCalls{}
		report, err = s.QueueReport(context.Background())
		if err == nil || !scheduler.GitHubFailure(err) {
			t.Fatalf("queue error = %v, want the remembered timeout", err)
		}
		if api.Calls.IssueReads != 0 {
			t.Fatalf("refused-issue reads = %d, want the timeout to back off for the window", api.Calls.IssueReads)
		}
		if len(report.Failures) != 1 || report.Failures[0].IssueNumber != 1 {
			t.Fatalf("queue failures = %+v, want the remembered timeout", report.Failures)
		}

	})
}

// A repository's share of the refresh budget falls below the read window once
// three or more repositories split it. A read that starts anyway gets the full
// window, so a GitHub hang still backs off for the whole window whatever the
// number of repositories.
func TestQueueBacksOffTheWholeWindowAfterAGenuineTimeoutAcrossRepositories(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {

		api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{}}
		now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
		gated := &testkit.DeadlineGitHub{FakeGitHub: api, SlowRepo: "owner/first"}
		s, runtime := testkit.ConfiguredAt(t, gated, "repositories:\n  - repo: owner/first\n  - repo: owner/second\n  - repo: owner/third\n", testkit.RejectingGit{}, func() time.Time { return now })
		// Only the first repository has a refused issue. The other two still split
		// the refresh budget, which is what used to shorten every read's window.
		testkit.RefuseIssues(t, runtime, api, []string{"owner/first"}, 1)

		refresh, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		report, err := s.QueueReport(refresh)
		cancel()
		if err == nil || !scheduler.GitHubFailure(err) {
			t.Fatalf("queue error = %v, want a scoped refused-issue timeout", err)
		}
		if api.Calls.IssueReads != 1 || len(report.Failures) != 1 || report.Failures[0].IssueNumber != 1 {
			t.Fatalf("reads = %d, failures = %+v, want one timeout", api.Calls.IssueReads, report.Failures)
		}

		// The next refresh falls inside the window, so the repository does not
		// read the issue again.
		now = now.Add(31 * time.Second)
		gated.SlowRepo = ""
		api.Calls = testkit.GithubCalls{}
		report, err = s.QueueReport(context.Background())
		if err == nil || !scheduler.GitHubFailure(err) {
			t.Fatalf("queue error = %v, want the remembered timeout", err)
		}
		if api.Calls.IssueReads != 0 {
			t.Fatalf("refused-issue reads = %d, want the timeout to back off for the window", api.Calls.IssueReads)
		}
		if len(report.Failures) != 1 || report.Failures[0].IssueNumber != 1 {
			t.Fatalf("queue failures = %+v, want the remembered timeout", report.Failures)
		}

	})
}

// A slow refusal must not always be read first. The refusals behind it still
// have to be read, or they never reach the queue and a closed one is never
// forgotten.
func TestQueueReadsTheRefusalsBehindASlowOne(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {

		repositories := []string{"owner/one", "owner/two", "owner/three", "owner/four"}
		api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{}}
		for _, repo := range repositories {
			api.Issues[repo] = []github.Issue{testkit.Ready(100)}
		}
		unfair := &testkit.UnfairGitHub{FakeGitHub: api, SlowRepo: "owner/one", SlowIssue: 1}
		now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
		s, runtime := testkit.ConfiguredAt(t, unfair, "repositories:\n  - repo: owner/one\n  - repo: owner/two\n  - repo: owner/three\n  - repo: owner/four\n", testkit.RejectingGit{}, func() time.Time { return now })
		// The first repository refuses two issues, and the lower-numbered one is
		// the slow one.
		for _, number := range []int{1, 2} {
			refused := testkit.Ready(number)
			refused.Labels = nil
			api.Issues["owner/one"] = append(api.Issues["owner/one"], refused)
			if _, err := runtime.DB.Exec("INSERT INTO issue_refusals(repository,issue_number,reason,ready_label,pending) VALUES('owner/one',?,'Untrusted label actor.','ready-for-agent',0)", number); err != nil {
				t.Fatal(err)
			}
		}
		// The first pass reads the slow refusal to its full window and remembers
		// the hang. The next pass starts after it, so the refusal behind it is
		// read instead of waiting behind the hang for another window.
		for pass := 1; pass <= 2; pass++ {
			now = now.Add(31 * time.Second)
			refresh, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, err := s.QueueReport(refresh)
			cancel()
			if err == nil || !scheduler.GitHubFailure(err) {
				t.Fatalf("pass %d queue error = %v, want the slow refusal to stay scoped", pass, err)
			}
		}
		if unfair.Reads == 0 {
			t.Fatal("the refusal behind the slow one was never read")
		}

	})
}

// A refresh that cannot give a refused read its full window starts none, even
// when three or more repositories split the budget. The next refresh reads
// them back.
func TestQueueDoesNotStartRefusedReadsTheRefreshCannotAffordAcrossRepositories(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {

		api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{}}
		now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
		gated := &testkit.DeadlineGitHub{FakeGitHub: api, SlowRepo: "owner/first"}
		s, runtime := testkit.ConfiguredAt(t, gated, "repositories:\n  - repo: owner/first\n  - repo: owner/second\n  - repo: owner/third\n", testkit.RejectingGit{}, func() time.Time { return now })
		testkit.RefuseIssues(t, runtime, api, []string{"owner/first", "owner/second", "owner/third"}, 1)

		first, cancelFirst := context.WithTimeout(context.Background(), 500*time.Millisecond)
		report, err := s.QueueReport(first)
		cancelFirst()
		if err != nil || api.Calls.IssueReads != 0 || len(report.Failures) != 0 {
			t.Fatalf("queue error = %v, reads = %d, failures = %+v; want no unaffordable read", err, api.Calls.IssueReads, report.Failures)
		}

		now = now.Add(31 * time.Second)
		gated.SlowRepo = ""
		api.Calls = testkit.GithubCalls{}
		report, err = s.QueueReport(context.Background())
		if err != nil {
			t.Fatalf("queue report = %v, want the next refresh to read the refusals", err)
		}
		if api.Calls.IssueReads != 3 || len(report.Issues) != 6 || len(report.Failures) != 0 {
			t.Fatalf("reads = %d, issues = %d, failures = %+v; want every refusal read next", api.Calls.IssueReads, len(report.Issues), report.Failures)
		}

	})
}

// A read the refresh budget cuts short keeps the last readable snapshot and is
// retried on the next refresh instead of being remembered as the issue's
// failure for a whole window.
func TestQueueRetriesARefusedReadCutShortByTheRefreshBudget(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {

		api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{}}
		now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
		started := make(chan struct{})
		gated := &testkit.DeadlineGitHub{FakeGitHub: api, Started: started}
		s, runtime := testkit.ConfiguredAt(t, gated, "repositories: [{repo: owner/first}]", testkit.RejectingGit{}, func() time.Time { return now })
		testkit.RefuseIssues(t, runtime, api, []string{"owner/first"}, 1)

		// Cache the refusal before the cut-short read.
		first, cancelFirst := context.WithTimeout(context.Background(), 30*time.Second)
		if _, err := s.QueueReport(first); err != nil {
			cancelFirst()
			t.Fatalf("first queue report = %v", err)
		}
		cancelFirst()

		// The snapshots expire and the read starts, but the refresh budget ends
		// before its full window.
		now = now.Add(16 * time.Minute)
		gated.SlowRepo = "owner/first"
		api.Calls = testkit.GithubCalls{}
		budget := &testkit.ExpiringRefreshContext{DeadlineHook: time.Now().Add(30 * time.Second), DoneHook: make(chan struct{})}
		done := make(chan error, 1)
		go func() {
			_, err := s.QueueReport(budget)
			done <- err
		}()
		<-started
		close(budget.DoneHook)
		if err := <-done; err != nil {
			t.Fatalf("cut-short queue error = %v, want the last snapshot kept", err)
		}
		if api.Calls.IssueReads != 1 {
			t.Fatalf("refused-issue reads = %d, want the cut-short read", api.Calls.IssueReads)
		}

		// The cut-short read backs off for one refresh, then is tried again.
		now = now.Add(31 * time.Second)
		gated.SlowRepo = ""
		api.Calls = testkit.GithubCalls{}
		report, err := s.QueueReport(context.Background())
		if err != nil || api.Calls.IssueReads != 1 || len(report.Issues) != 2 {
			t.Fatalf("retry queue = %d issues, reads = %d, err = %v; want the refusal read again", len(report.Issues), api.Calls.IssueReads, err)
		}

	})
}

// A refresh too short to give a refused read its full window does not start
// it. The issue is read again on the next refresh instead of being remembered
// as a failure for a whole window.
func TestQueueDoesNotStartARefusedReadTheRefreshCannotAfford(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {

		api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{}}
		now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
		gated := &testkit.DeadlineGitHub{FakeGitHub: api, SlowRepo: "owner/first"}
		s, runtime := testkit.ConfiguredAt(t, gated, "repositories: [{repo: owner/first}]", testkit.RejectingGit{}, func() time.Time { return now })
		testkit.RefuseIssues(t, runtime, api, []string{"owner/first"}, 1)

		// The refresh cannot afford a read with a full window, so none starts.
		first, cancelFirst := context.WithTimeout(context.Background(), 500*time.Millisecond)
		report, err := s.QueueReport(first)
		cancelFirst()
		if err != nil || api.Calls.IssueReads != 0 || len(report.Failures) != 0 {
			t.Fatalf("queue error = %v, reads = %d, failures = %+v; want no unaffordable read", err, api.Calls.IssueReads, report.Failures)
		}

		// The next refresh can afford the read, so the refusal is read and shown
		// instead of staying hidden for a whole window.
		now = now.Add(31 * time.Second)
		gated.SlowRepo = ""
		api.Calls = testkit.GithubCalls{}
		report, err = s.QueueReport(context.Background())
		if err != nil {
			t.Fatalf("queue report = %v, want the retried read to succeed", err)
		}
		if api.Calls.IssueReads != 1 {
			t.Fatalf("refused-issue reads = %d, want the refused read the next refresh", api.Calls.IssueReads)
		}
		if len(report.Issues) != 2 || report.Issues[0].Issue.Number != 1 || report.Issues[0].RefusalReason == "" {
			t.Fatalf("the retried refusal stayed out of the queue: %+v", report.Issues)
		}

	})
}
