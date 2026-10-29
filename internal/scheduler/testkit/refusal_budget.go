package testkit

import (
	"context"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
)

// slowGitHub delays every refused-issue read, so a test can reproduce a
// refresh whose budget runs out while the refusals are read back.
type SlowGitHub struct {
	*FakeGitHub
	Delay time.Duration
}

// GetIssue answers after the delay, and reports the adapter's timeout code
// when the read's own deadline expires first.
func (g *SlowGitHub) GetIssue(ctx context.Context, repo string, number int) (github.Issue, error) {
	select {
	case <-time.After(g.Delay):
		return g.FakeGitHub.GetIssue(ctx, repo, number)
	case <-ctx.Done():
		g.FakeGitHub.Calls.IssueReads++
		return github.Issue{}, &fault.Error{Code: "github.timeout", Message: "The GitHub request timed out; it will retry", Err: ctx.Err()}
	}
}

// refuseIssues gives every listed repository one ordinary ready issue and the
// given number of refused issues, and records the refusals.
func RefuseIssues(t *testing.T, runtime *app.Runtime, api *FakeGitHub, repositories []string, refusals int) {
	t.Helper()
	for index, repo := range repositories {
		api.Issues[repo] = []github.Issue{Ready(100 + index)}
		for number := 1; number <= refusals; number++ {
			refused := Ready(number)
			refused.Labels = nil
			api.Issues[repo] = append(api.Issues[repo], refused)
			if _, err := runtime.DB.Exec("INSERT INTO issue_refusals(repository,issue_number,reason,ready_label,pending) VALUES(?,?,'Untrusted label actor.','ready-for-agent',0)", repo, number); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// unfairGitHub makes one refused issue of one repository block until the
// read's deadline, so it spends that repository's whole allowance. The other
// refused issues are read at once.
type UnfairGitHub struct {
	*FakeGitHub
	SlowRepo  string
	SlowIssue int
	Reads     int
}

func (g *UnfairGitHub) GetIssue(ctx context.Context, repo string, number int) (github.Issue, error) {
	if repo == g.SlowRepo && number == g.SlowIssue {
		<-ctx.Done()
		g.FakeGitHub.Calls.IssueReads++
		return github.Issue{}, &fault.Error{Code: "github.timeout", Message: "The GitHub request timed out; it will retry", Err: ctx.Err()}
	}
	g.Reads++
	return g.FakeGitHub.GetIssue(ctx, repo, number)
}

// expiringRefreshContext has a deadline far enough away to start a full read,
// but ends early like a refresh budget that other work spends before the read
// finishes.
type ExpiringRefreshContext struct {
	DeadlineHook time.Time
	DoneHook     chan struct{}
}

func (c *ExpiringRefreshContext) Deadline() (time.Time, bool) { return c.DeadlineHook, true }
func (c *ExpiringRefreshContext) Done() <-chan struct{}       { return c.DoneHook }
func (c *ExpiringRefreshContext) Value(any) any               { return nil }

func (c *ExpiringRefreshContext) Err() error {
	select {
	case <-c.DoneHook:
		return context.DeadlineExceeded
	default:
		return nil
	}
}
