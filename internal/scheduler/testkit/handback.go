package testkit

import (
	"context"

	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
)

type InterruptedHandbackGit struct {
	*managedgit.Manager
	Boundary string
	Cancel   context.CancelFunc
}

func (g InterruptedHandbackGit) CommitHandback(ctx context.Context, run managedgit.Run, previous string) (managedgit.CommitResult, error) {
	result, err := g.Manager.CommitHandback(ctx, run, previous)
	if err == nil && g.Boundary == "commit" {
		g.Cancel()
		return result, ctx.Err()
	}
	return result, err
}
func (g InterruptedHandbackGit) PushHandback(ctx context.Context, run managedgit.Run, previous, target string) error {
	err := g.Manager.PushHandback(ctx, run, previous, target)
	if err == nil && g.Boundary == "push" {
		g.Cancel()
		return ctx.Err()
	}
	return err
}

type HandbackDiscoveryGitHub struct{ *FakeGitHub }

func (g HandbackDiscoveryGitHub) FindOpenPullRequest(ctx context.Context, repo, branch string) (*github.PullRequest, error) {
	pr, err := g.FakeGitHub.FindOpenPullRequest(ctx, repo, branch)
	if err == nil && pr != nil && pr.State != github.Open {
		return nil, nil
	}
	return pr, err
}

type AmbiguousHandbackGitHub struct{ *FakeGitHub }

func (g AmbiguousHandbackGitHub) FindPullRequest(context.Context, string, string) (*github.PullRequest, error) {
	return nil, &fault.Error{Code: "pr.multiple_matches", Message: "more than one matching PR across states"}
}
