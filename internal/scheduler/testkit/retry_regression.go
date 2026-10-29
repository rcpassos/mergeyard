package testkit

import (
	"context"
	"errors"

	"github.com/rcpassos/mergeyard/internal/github"
)

type RetryStopGitHub struct {
	*FakeGitHub
	Cancel  context.CancelFunc
	Offline bool
}

func (f *RetryStopGitHub) GetPullRequest(ctx context.Context, repo string, n int) (*github.PullRequest, error) {
	if f.Cancel != nil {
		f.Cancel()
		return nil, ctx.Err()
	}
	if f.Offline {
		return nil, errors.New("PR lookup unavailable")
	}
	return f.FakeGitHub.GetPullRequest(ctx, repo, n)
}
