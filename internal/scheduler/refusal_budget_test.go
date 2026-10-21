package scheduler_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
)

// slowGitHub delays every refused-issue read, so a test can reproduce a
// refresh whose budget runs out while the refusals are read back.
type slowGitHub struct {
	*fakeGitHub
	delay time.Duration
}

// GetIssue answers after the delay, and reports the adapter's timeout code
// when the read's own deadline expires first.
func (g *slowGitHub) GetIssue(ctx context.Context, repo string, number int) (github.Issue, error) {
	select {
	case <-time.After(g.delay):
		return g.fakeGitHub.GetIssue(ctx, repo, number)
	case <-ctx.Done():
		g.fakeGitHub.calls.issueReads++
		return github.Issue{}, &fault.Error{Code: "github.timeout", Message: "The GitHub request timed out; it will retry", Err: ctx.Err()}
	}
}

// refuseIssues gives every listed repository one ordinary ready issue and the
// given number of refused issues, and records the refusals.
func refuseIssues(t *testing.T, runtime *app.Runtime, api *fakeGitHub, repositories []string, refusals int) {
	t.Helper()
	for index, repo := range repositories {
		api.issues[repo] = []github.Issue{ready(100 + index)}
		for number := 1; number <= refusals; number++ {
			refused := ready(number)
			refused.Labels = nil
			api.issues[repo] = append(api.issues[repo], refused)
			if _, err := runtime.DB.Exec("INSERT INTO issue_refusals(repository,issue_number,reason,ready_label,pending) VALUES(?,?,'Untrusted label actor.','ready-for-agent',0)", repo, number); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// Thirty refused issues that expire together must not spend the whole
// refresh. A refresh too short to give a read its full window starts none of
// them, and the pass still returns every repository's queue from the last
// readable snapshots.
func TestQueueKeepsEveryRepositoryWhenRefusedIssuesExpireTogether(t *testing.T) {
	const refusals = 15
	api := &fakeGitHub{issues: map[string][]github.Issue{}}
	slow := &slowGitHub{fakeGitHub: api, delay: 30 * time.Millisecond}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s, runtime := configuredAt(t, slow, "repositories:\n  - repo: owner/first\n  - repo: owner/second\n", rejectingGit{}, func() time.Time { return now })
	refuseIssues(t, runtime, api, []string{"owner/first", "owner/second"}, refusals)

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
	if api.calls.issueReads != 2*refusals {
		t.Fatalf("first refused-issue reads = %d, want %d", api.calls.issueReads, 2*refusals)
	}

	// The snapshots expire together, but the next refresh is too short to
	// give a refused read its full window, so it keeps every snapshot.
	now = now.Add(16 * time.Minute)
	api.calls = githubCalls{}
	second, cancelSecond := context.WithTimeout(context.Background(), 600*time.Millisecond)
	report, err = s.QueueReport(second)
	cancelSecond()
	if err != nil {
		t.Fatalf("second queue report = %v, want a partial queue instead of a failed one", err)
	}
	if len(report.Issues) != 2*refusals+2 {
		t.Fatalf("second queue = %d issues, want %d: the refresh dropped the queue", len(report.Issues), 2*refusals+2)
	}
	if api.calls.issueReads != 0 {
		t.Fatalf("refused-issue reads = %d, want no read the refresh cannot afford", api.calls.issueReads)
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
}

// A refresh that cannot give a refused read its full window does not start it.
// The refusal keeps its last readable snapshot, and the next refresh reads it
// instead of backing off for a whole window.
func TestQueueKeepsSnapshotWhenTheRefreshCannotAffordARead(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{}}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s, runtime := configuredAt(t, api, "repositories: [{repo: owner/first}]", rejectingGit{}, func() time.Time { return now })
	refuseIssues(t, runtime, api, []string{"owner/first"}, 2)

	first, cancelFirst := context.WithTimeout(context.Background(), 30*time.Second)
	report, err := s.QueueReport(first)
	cancelFirst()
	if err != nil || len(report.Issues) != 3 || api.calls.issueReads != 2 {
		t.Fatalf("first queue = %d issues, reads = %d, err = %v; want both refusals read", len(report.Issues), api.calls.issueReads, err)
	}

	// Both snapshots expire together, but the refresh cannot afford a read
	// with a full window, so neither is started.
	now = now.Add(16 * time.Minute)
	api.calls = githubCalls{}
	second, cancelSecond := context.WithTimeout(context.Background(), 500*time.Millisecond)
	report, err = s.QueueReport(second)
	cancelSecond()
	if err != nil {
		t.Fatalf("second queue error = %v, want the snapshots kept", err)
	}
	if api.calls.issueReads != 0 {
		t.Fatalf("refused-issue reads = %d, want no read the refresh cannot afford", api.calls.issueReads)
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
	api.calls = githubCalls{}
	report, err = s.QueueReport(context.Background())
	if err != nil || api.calls.issueReads != 2 || len(report.Issues) != 3 {
		t.Fatalf("retry queue = %d issues, reads = %d, err = %v; want the kept refusals read", len(report.Issues), api.calls.issueReads, err)
	}
}

// A refresh that cannot afford refused reads still assembles the queue from
// their snapshots: the local lookups do not share the budget reserved for
// GitHub.
func TestQueueKeepsRefusalReasonsWhenTheRefreshCannotAffordReads(t *testing.T) {
	const refusals = 15
	api := &fakeGitHub{issues: map[string][]github.Issue{}}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s, runtime := configuredAt(t, api, "repositories: [{repo: owner/first}]", rejectingGit{}, func() time.Time { return now })
	refuseIssues(t, runtime, api, []string{"owner/first"}, refusals)

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
}

// A slow refused read that fits its window stays scoped to its issue. The
// repositories after it are still listed instead of losing their queue to a
// spent GitHub budget.
func TestQueueKeepsDiscoveringAfterASlowRefusedRead(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{}}
	gated := &deadlineGitHub{fakeGitHub: api, slowRepo: "owner/first"}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s, runtime := configuredAt(t, gated, "repositories:\n  - repo: owner/first\n  - repo: owner/second\n", rejectingGit{}, func() time.Time { return now })
	refuseIssues(t, runtime, api, []string{"owner/first", "owner/second"}, 3)

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
}

// A read that blocks until its own window ends is a genuine GitHub hang, like
// the real adapter reports. It backs off for the full window, so an outage
// does not spend one call per refused issue on every refresh.
func TestQueueBacksOffTheWholeWindowAfterAGenuineTimeout(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{}}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	gated := &deadlineGitHub{fakeGitHub: api, slowRepo: "owner/first"}
	s, runtime := configuredAt(t, gated, "repositories: [{repo: owner/first}]", rejectingGit{}, func() time.Time { return now })
	refuseIssues(t, runtime, api, []string{"owner/first"}, 1)

	// The read reaches its own full window before the refresh budget ends,
	// so the timeout belongs to GitHub and not to the budget.
	refresh, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	report, err := s.QueueReport(refresh)
	cancel()
	if err == nil || !scheduler.GitHubFailure(err) {
		t.Fatalf("queue error = %v, want a scoped refused-issue timeout", err)
	}
	if api.calls.issueReads != 1 || len(report.Failures) != 1 || report.Failures[0].IssueNumber != 1 {
		t.Fatalf("reads = %d, queue failures = %+v, want one timeout", api.calls.issueReads, report.Failures)
	}

	// The failure backs off for the whole window instead of every refresh.
	now = now.Add(31 * time.Second)
	gated.slowRepo = ""
	api.calls = githubCalls{}
	report, err = s.QueueReport(context.Background())
	if err == nil || !scheduler.GitHubFailure(err) {
		t.Fatalf("queue error = %v, want the remembered timeout", err)
	}
	if api.calls.issueReads != 0 {
		t.Fatalf("refused-issue reads = %d, want the timeout to back off for the window", api.calls.issueReads)
	}
	if len(report.Failures) != 1 || report.Failures[0].IssueNumber != 1 {
		t.Fatalf("queue failures = %+v, want the remembered timeout", report.Failures)
	}
}

// A repository's share of the refresh budget falls below the read window once
// three or more repositories split it. A read that starts anyway gets the full
// window, so a GitHub hang still backs off for the whole window whatever the
// number of repositories.
func TestQueueBacksOffTheWholeWindowAfterAGenuineTimeoutAcrossRepositories(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{}}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	gated := &deadlineGitHub{fakeGitHub: api, slowRepo: "owner/first"}
	s, runtime := configuredAt(t, gated, "repositories:\n  - repo: owner/first\n  - repo: owner/second\n  - repo: owner/third\n", rejectingGit{}, func() time.Time { return now })
	// Only the first repository has a refused issue. The other two still split
	// the refresh budget, which is what used to shorten every read's window.
	refuseIssues(t, runtime, api, []string{"owner/first"}, 1)

	refresh, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	report, err := s.QueueReport(refresh)
	cancel()
	if err == nil || !scheduler.GitHubFailure(err) {
		t.Fatalf("queue error = %v, want a scoped refused-issue timeout", err)
	}
	if api.calls.issueReads != 1 || len(report.Failures) != 1 || report.Failures[0].IssueNumber != 1 {
		t.Fatalf("reads = %d, failures = %+v, want one timeout", api.calls.issueReads, report.Failures)
	}

	// The next refresh falls inside the window, so the repository does not
	// read the issue again.
	now = now.Add(31 * time.Second)
	gated.slowRepo = ""
	api.calls = githubCalls{}
	report, err = s.QueueReport(context.Background())
	if err == nil || !scheduler.GitHubFailure(err) {
		t.Fatalf("queue error = %v, want the remembered timeout", err)
	}
	if api.calls.issueReads != 0 {
		t.Fatalf("refused-issue reads = %d, want the timeout to back off for the window", api.calls.issueReads)
	}
	if len(report.Failures) != 1 || report.Failures[0].IssueNumber != 1 {
		t.Fatalf("queue failures = %+v, want the remembered timeout", report.Failures)
	}
}

// unfairGitHub makes one refused issue of one repository block until the
// read's deadline, so it spends that repository's whole allowance. The other
// refused issues are read at once.
type unfairGitHub struct {
	*fakeGitHub
	slowRepo  string
	slowIssue int
	reads     int
}

func (g *unfairGitHub) GetIssue(ctx context.Context, repo string, number int) (github.Issue, error) {
	if repo == g.slowRepo && number == g.slowIssue {
		<-ctx.Done()
		g.fakeGitHub.calls.issueReads++
		return github.Issue{}, &fault.Error{Code: "github.timeout", Message: "The GitHub request timed out; it will retry", Err: ctx.Err()}
	}
	g.reads++
	return g.fakeGitHub.GetIssue(ctx, repo, number)
}

// A slow refusal must not always be read first. The refusals behind it still
// have to be read, or they never reach the queue and a closed one is never
// forgotten.
func TestQueueReadsTheRefusalsBehindASlowOne(t *testing.T) {
	repositories := []string{"owner/one", "owner/two", "owner/three", "owner/four"}
	api := &fakeGitHub{issues: map[string][]github.Issue{}}
	for _, repo := range repositories {
		api.issues[repo] = []github.Issue{ready(100)}
	}
	unfair := &unfairGitHub{fakeGitHub: api, slowRepo: "owner/one", slowIssue: 1}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s, runtime := configuredAt(t, unfair, "repositories:\n  - repo: owner/one\n  - repo: owner/two\n  - repo: owner/three\n  - repo: owner/four\n", rejectingGit{}, func() time.Time { return now })
	// The first repository refuses two issues, and the lower-numbered one is
	// the slow one.
	for _, number := range []int{1, 2} {
		refused := ready(number)
		refused.Labels = nil
		api.issues["owner/one"] = append(api.issues["owner/one"], refused)
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
	if unfair.reads == 0 {
		t.Fatal("the refusal behind the slow one was never read")
	}
}

// A refresh that cannot give a refused read its full window starts none, even
// when three or more repositories split the budget. The next refresh reads
// them back.
func TestQueueDoesNotStartRefusedReadsTheRefreshCannotAffordAcrossRepositories(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{}}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	gated := &deadlineGitHub{fakeGitHub: api, slowRepo: "owner/first"}
	s, runtime := configuredAt(t, gated, "repositories:\n  - repo: owner/first\n  - repo: owner/second\n  - repo: owner/third\n", rejectingGit{}, func() time.Time { return now })
	refuseIssues(t, runtime, api, []string{"owner/first", "owner/second", "owner/third"}, 1)

	first, cancelFirst := context.WithTimeout(context.Background(), 500*time.Millisecond)
	report, err := s.QueueReport(first)
	cancelFirst()
	if err != nil || api.calls.issueReads != 0 || len(report.Failures) != 0 {
		t.Fatalf("queue error = %v, reads = %d, failures = %+v; want no unaffordable read", err, api.calls.issueReads, report.Failures)
	}

	now = now.Add(31 * time.Second)
	gated.slowRepo = ""
	api.calls = githubCalls{}
	report, err = s.QueueReport(context.Background())
	if err != nil {
		t.Fatalf("queue report = %v, want the next refresh to read the refusals", err)
	}
	if api.calls.issueReads != 3 || len(report.Issues) != 6 || len(report.Failures) != 0 {
		t.Fatalf("reads = %d, issues = %d, failures = %+v; want every refusal read next", api.calls.issueReads, len(report.Issues), report.Failures)
	}
}

// expiringRefreshContext has a deadline far enough away to start a full read,
// but ends early like a refresh budget that other work spends before the read
// finishes.
type expiringRefreshContext struct {
	deadline time.Time
	done     chan struct{}
}

func (c *expiringRefreshContext) Deadline() (time.Time, bool) { return c.deadline, true }
func (c *expiringRefreshContext) Done() <-chan struct{}       { return c.done }
func (c *expiringRefreshContext) Value(any) any               { return nil }

func (c *expiringRefreshContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// A read the refresh budget cuts short keeps the last readable snapshot and is
// retried on the next refresh instead of being remembered as the issue's
// failure for a whole window.
func TestQueueRetriesARefusedReadCutShortByTheRefreshBudget(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{}}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	started := make(chan struct{})
	gated := &deadlineGitHub{fakeGitHub: api, started: started}
	s, runtime := configuredAt(t, gated, "repositories: [{repo: owner/first}]", rejectingGit{}, func() time.Time { return now })
	refuseIssues(t, runtime, api, []string{"owner/first"}, 1)

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
	gated.slowRepo = "owner/first"
	api.calls = githubCalls{}
	budget := &expiringRefreshContext{deadline: time.Now().Add(30 * time.Second), done: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := s.QueueReport(budget)
		done <- err
	}()
	<-started
	close(budget.done)
	if err := <-done; err != nil {
		t.Fatalf("cut-short queue error = %v, want the last snapshot kept", err)
	}
	if api.calls.issueReads != 1 {
		t.Fatalf("refused-issue reads = %d, want the cut-short read", api.calls.issueReads)
	}

	// The cut-short read backs off for one refresh, then is tried again.
	now = now.Add(31 * time.Second)
	gated.slowRepo = ""
	api.calls = githubCalls{}
	report, err := s.QueueReport(context.Background())
	if err != nil || api.calls.issueReads != 1 || len(report.Issues) != 2 {
		t.Fatalf("retry queue = %d issues, reads = %d, err = %v; want the refusal read again", len(report.Issues), api.calls.issueReads, err)
	}
}

// A refresh too short to give a refused read its full window does not start
// it. The issue is read again on the next refresh instead of being remembered
// as a failure for a whole window.
func TestQueueDoesNotStartARefusedReadTheRefreshCannotAfford(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{}}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	gated := &deadlineGitHub{fakeGitHub: api, slowRepo: "owner/first"}
	s, runtime := configuredAt(t, gated, "repositories: [{repo: owner/first}]", rejectingGit{}, func() time.Time { return now })
	refuseIssues(t, runtime, api, []string{"owner/first"}, 1)

	// The refresh cannot afford a read with a full window, so none starts.
	first, cancelFirst := context.WithTimeout(context.Background(), 500*time.Millisecond)
	report, err := s.QueueReport(first)
	cancelFirst()
	if err != nil || api.calls.issueReads != 0 || len(report.Failures) != 0 {
		t.Fatalf("queue error = %v, reads = %d, failures = %+v; want no unaffordable read", err, api.calls.issueReads, report.Failures)
	}

	// The next refresh can afford the read, so the refusal is read and shown
	// instead of staying hidden for a whole window.
	now = now.Add(31 * time.Second)
	gated.slowRepo = ""
	api.calls = githubCalls{}
	report, err = s.QueueReport(context.Background())
	if err != nil {
		t.Fatalf("queue report = %v, want the retried read to succeed", err)
	}
	if api.calls.issueReads != 1 {
		t.Fatalf("refused-issue reads = %d, want the refused read the next refresh", api.calls.issueReads)
	}
	if len(report.Issues) != 2 || report.Issues[0].Issue.Number != 1 || report.Issues[0].RefusalReason == "" {
		t.Fatalf("the retried refusal stayed out of the queue: %+v", report.Issues)
	}
}
