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

// overrunningGitHub answers a refused-issue read only after a fixed delay,
// ignoring the read's own deadline. It reproduces a GitHub call that keeps
// the caller waiting after its window has passed.
type overrunningGitHub struct {
	*fakeGitHub
	delay time.Duration
}

func (g *overrunningGitHub) GetIssue(ctx context.Context, repo string, number int) (github.Issue, error) {
	time.Sleep(g.delay)
	return g.fakeGitHub.GetIssue(ctx, repo, number)
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
// refresh. The pass returns every repository's queue, keeps the last readable
// snapshots for the refusals it could not read back, and reads back only as
// many refusals as the budget allows.
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

	// The snapshots expire together, and the next refresh reads them back
	// within a budget that cannot afford all of them.
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
	if api.calls.issueReads == 0 {
		t.Fatal("the refresh read no refused issue back")
	}
	if api.calls.issueReads >= 2*refusals {
		t.Fatalf("the refresh read every refused issue back: %d reads", api.calls.issueReads)
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

// A refresh that spends its GitHub budget on the refused reads still assembles
// the queue: the local lookups do not share the budget reserved for GitHub.
func TestQueueKeepsRefusalReasonsAfterTheGitHubBudgetIsSpent(t *testing.T) {
	const refusals = 15
	api := &fakeGitHub{issues: map[string][]github.Issue{}}
	overrunning := &overrunningGitHub{fakeGitHub: api}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s, runtime := configuredAt(t, overrunning, "repositories: [{repo: owner/first}]", rejectingGit{}, func() time.Time { return now })
	refuseIssues(t, runtime, api, []string{"owner/first"}, refusals)

	// The first pass caches the snapshots of every refused issue.
	first, cancelFirst := context.WithTimeout(context.Background(), 10*time.Second)
	report, err := s.QueueReport(first)
	cancelFirst()
	if err != nil {
		t.Fatalf("first queue report = %v, want a complete queue", err)
	}
	if len(report.Issues) != refusals+1 {
		t.Fatalf("first queue = %d issues, want %d", len(report.Issues), refusals+1)
	}

	// The snapshots expire together and the next refresh spends its whole
	// budget on the first read back.
	now = now.Add(16 * time.Minute)
	overrunning.delay = 700 * time.Millisecond
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

// A refresh that spends its budget on the first repository's refused reads
// still reports the repository it could not list, instead of failing the
// whole queue with a plain deadline error.
func TestQueueKeepsDiscoveringWhenTheBudgetRunsOutBeforeAListing(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{}}
	overrunning := &overrunningGitHub{fakeGitHub: api, delay: 700 * time.Millisecond}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s, runtime := configuredAt(t, overrunning, "repositories:\n  - repo: owner/first\n  - repo: owner/second\n", rejectingGit{}, func() time.Time { return now })
	refuseIssues(t, runtime, api, []string{"owner/first", "owner/second"}, 3)

	refresh, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	report, err := s.QueueReport(refresh)
	cancel()
	if !scheduler.GitHubFailure(err) {
		t.Fatalf("queue error = %v, want a scoped repository failure instead of a failed queue", err)
	}
	if len(report.Failures) != 1 || report.Failures[0].Repository != "owner/second" {
		t.Fatalf("queue failures = %+v, want the repository that could not be listed", report.Failures)
	}
	if len(report.Issues) != 2 || !strings.EqualFold(report.Issues[0].Repository, "owner/first") {
		t.Fatalf("the discovered repository lost its queue: %+v", report.Issues)
	}
}

// A read that used its whole window is a genuine failure rather than a slow
// one: it backs off for the full window, so an outage does not spend one call
// per refused issue on every refresh.
func TestQueueBacksOffTheWholeWindowAfterAGenuineTimeout(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{}}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s, runtime := configuredAt(t, api, "repositories: [{repo: owner/first}]", rejectingGit{}, func() time.Time { return now })
	refuseIssues(t, runtime, api, []string{"owner/first"}, 1)

	// The refresh budget leaves the read its whole window, so the timeout
	// belongs to GitHub and not to the budget.
	api.getIssue = func(string, int) (github.Issue, error) {
		return github.Issue{}, &fault.Error{Code: "github.timeout", Message: "The GitHub request timed out; it will retry"}
	}
	refresh, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	report, err := s.QueueReport(refresh)
	cancel()
	if err == nil || !scheduler.GitHubFailure(err) {
		t.Fatalf("queue error = %v, want a scoped refused-issue timeout", err)
	}
	if len(report.Failures) != 1 || report.Failures[0].IssueNumber != 1 {
		t.Fatalf("queue failures = %+v", report.Failures)
	}

	// The failure backs off for the whole window instead of every refresh.
	now = now.Add(31 * time.Second)
	api.getIssue = nil
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
// three or more repositories split it. A timeout that GitHub reports before the
// budget ends the read is still GitHub's own, so it backs off for the whole
// window whatever the number of repositories.
func TestQueueBacksOffTheWholeWindowAfterAGenuineTimeoutAcrossRepositories(t *testing.T) {
	repositories := []string{"owner/first", "owner/second", "owner/third"}
	api := &fakeGitHub{issues: map[string][]github.Issue{}}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s, runtime := configuredAt(t, api, "repositories:\n  - repo: owner/first\n  - repo: owner/second\n  - repo: owner/third\n", rejectingGit{}, func() time.Time { return now })
	refuseIssues(t, runtime, api, repositories, 1)

	reads := 0
	api.getIssue = func(string, int) (github.Issue, error) {
		reads++
		return github.Issue{}, &fault.Error{Code: "github.timeout", Message: "The GitHub request timed out; it will retry"}
	}
	refresh, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	report, err := s.QueueReport(refresh)
	cancel()
	if err == nil || !scheduler.GitHubFailure(err) {
		t.Fatalf("queue error = %v, want scoped refused-issue timeouts", err)
	}
	if reads != len(repositories) || len(report.Failures) != len(repositories) {
		t.Fatalf("reads = %d, failures = %+v, want one timeout per repository", reads, report.Failures)
	}

	// The next refresh falls inside the window, so no repository reads again.
	now = now.Add(31 * time.Second)
	reads = 0
	report, err = s.QueueReport(context.Background())
	if err == nil || !scheduler.GitHubFailure(err) {
		t.Fatalf("queue error = %v, want the remembered timeouts", err)
	}
	if reads != 0 {
		t.Fatalf("refused-issue reads = %d, want the timeouts to back off for the window", reads)
	}
	if len(report.Failures) != len(repositories) {
		t.Fatalf("queue failures = %+v, want the remembered timeouts", report.Failures)
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
	// The first repository's share of a 200ms refresh is 25ms, which the slow
	// refusal spends on every pass.
	for pass := 1; pass <= 4; pass++ {
		now = now.Add(31 * time.Second)
		refresh, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
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

// A read that was merely slow is not remembered as the issue's failure for a
// whole window: the next refresh reads the issue back and shows it again.
func TestQueueRetriesASlowRefusedReadOnTheNextRefresh(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{}}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	gated := &deadlineGitHub{fakeGitHub: api, slowRepo: "owner/first"}
	s, runtime := configuredAt(t, gated, "repositories: [{repo: owner/first}]", rejectingGit{}, func() time.Time { return now })
	refuseIssues(t, runtime, api, []string{"owner/first"}, 1)

	// The refresh budget cuts the read short, so the read is merely slow.
	first, cancelFirst := context.WithTimeout(context.Background(), 500*time.Millisecond)
	report, err := s.QueueReport(first)
	cancelFirst()
	if err == nil || !scheduler.GitHubFailure(err) {
		t.Fatalf("queue error = %v, want a scoped refused-issue timeout", err)
	}
	if len(report.Failures) != 1 || report.Failures[0].IssueNumber != 1 {
		t.Fatalf("queue failures = %+v", report.Failures)
	}

	// The slow read is retried on the next refresh instead of being cached
	// as the issue's state for a whole window.
	now = now.Add(31 * time.Second)
	gated.slowRepo = ""
	api.calls = githubCalls{}
	report, err = s.QueueReport(context.Background())
	if err != nil {
		t.Fatalf("queue report = %v, want the retried read to succeed", err)
	}
	if api.calls.issueReads != 1 {
		t.Fatalf("refused-issue reads = %d, want the slow read to be retried", api.calls.issueReads)
	}
	if len(report.Issues) != 2 || report.Issues[0].Issue.Number != 1 || report.Issues[0].RefusalReason == "" {
		t.Fatalf("the retried refusal stayed out of the queue: %+v", report.Issues)
	}
}
