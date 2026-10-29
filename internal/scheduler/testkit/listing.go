package testkit

import (
	"context"
	"database/sql"
	"errors"
	"sync"

	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
)

// deadlineGitHub blocks refused-issue reads for one repository until the
// caller's deadline, then reports the adapter's timeout.
type DeadlineGitHub struct {
	*FakeGitHub
	SlowRepo string
	Started  chan struct{}
}

func (g *DeadlineGitHub) GetIssue(ctx context.Context, repo string, number int) (github.Issue, error) {
	if repo != g.SlowRepo {
		return g.FakeGitHub.GetIssue(ctx, repo, number)
	}
	if g.Started != nil {
		close(g.Started)
		g.Started = nil
	}
	<-ctx.Done()
	g.FakeGitHub.Calls.IssueReads++
	if errors.Is(ctx.Err(), context.Canceled) {
		return github.Issue{}, &fault.Error{Code: "github.canceled", Message: "The GitHub request was canceled; it will retry", Err: ctx.Err()}
	}
	return github.Issue{}, &fault.Error{Code: "github.timeout", Message: "The GitHub request timed out; it will retry", Err: ctx.Err()}
}

// stallListing blocks the first label-filtered listing fetch until unblock is
// called, so a test can hold the shared listing gate.
func StallListing(api *FakeGitHub) (<-chan struct{}, func()) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	stalled := false
	api.ListHook = func() {
		if !stalled {
			stalled = true
			close(started)
			<-release
		}
	}
	return started, func() { once.Do(func() { close(release) }) }
}

func CountRefusals(db *sql.DB) (int, error) {
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM issue_refusals").Scan(&count)
	return count, err
}
