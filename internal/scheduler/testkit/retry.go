package testkit

import (
	"context"

	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
)

type RetryMergeGitHub struct{ *MergeGitHub }

func (f *RetryMergeGitHub) GetPullRequest(ctx context.Context, repo string, n int) (*github.PullRequest, error) {
	pr, err := f.FakeGitHub.GetPullRequest(ctx, repo, n)
	if err == nil && pr != nil {
		pr.Head.SHA = f.Head(pr.Head.Ref)
	}
	return pr, err
}

type RetryCancelGit struct {
	*managedgit.Manager
	Cancel context.CancelFunc
}

func (g *RetryCancelGit) InspectRetry(ctx context.Context, run managedgit.Run, target string, edits bool) (string, error) {
	if g.Cancel != nil {
		g.Cancel()
		return "", ctx.Err()
	}
	return g.Manager.InspectRetry(ctx, run, target, edits)
}

type RetryPublicationGitHub struct {
	*FakeGitHub
	Unavailable bool
}

func (f *RetryPublicationGitHub) CreateDraftPullRequest(ctx context.Context, repo, branch, base string, content github.PullRequestContent) (*github.PullRequest, error) {
	if f.Unavailable {
		return nil, &fault.Error{Code: "github.offline", Message: "Publication temporarily unavailable"}
	}
	return f.FakeGitHub.CreateDraftPullRequest(ctx, repo, branch, base, content)
}

type RetryErrorGit struct {
	*managedgit.Manager
	Failure error
}

func (g *RetryErrorGit) InspectRetry(ctx context.Context, run managedgit.Run, target string, edits bool) (string, error) {
	if g.Failure != nil {
		return "", g.Failure
	}
	return g.Manager.InspectRetry(ctx, run, target, edits)
}
