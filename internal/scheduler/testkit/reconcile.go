package testkit

import (
	"context"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
)

type ControlPlaneInput struct {
	Config    config.Config
	Workspace string
	Socket    string
}

// handbackGit satisfies HandbackGit; the PR read fails before publication, so
// every publication call is a bug.
type HandbackGit struct{ RejectingGit }

func (HandbackGit) InspectHandback(context.Context, managedgit.Run, string) (string, error) {
	panic("handback inspection must not run when the PR read fails")
}
func (HandbackGit) CommitHandback(context.Context, managedgit.Run, string) (managedgit.CommitResult, error) {
	panic("handback commit must not run when the PR read fails")
}
func (HandbackGit) PushHandback(context.Context, managedgit.Run, string, string) error {
	panic("handback push must not run when the PR read fails")
}

type UnavailablePullRequests struct{ *FakeGitHub }

func (UnavailablePullRequests) GetPullRequest(context.Context, string, int) (*github.PullRequest, error) {
	return nil, &fault.Error{Code: "github.unavailable", Message: "dial tcp: connection refused"}
}
func (UnavailablePullRequests) FindPullRequest(context.Context, string, string) (*github.PullRequest, error) {
	return nil, &fault.Error{Code: "github.unavailable", Message: "dial tcp: connection refused"}
}

type ToggledGitHub struct {
	*FakeGitHub
	Fail bool
}

func (g *ToggledGitHub) ListLabeledIssues(ctx context.Context, repo string, labels []string) ([]github.Issue, error) {
	if g.Fail {
		return nil, &fault.Error{Code: "github.unavailable", Message: "dial tcp: connection refused"}
	}
	return g.FakeGitHub.ListLabeledIssues(ctx, repo, labels)
}

// repositoryFailureGitHub fails listings for selected repositories and leaves
// every other boundary call to the normal fake.
type RepositoryFailureGitHub struct {
	*FakeGitHub
	Failures map[string]error
}

func (g *RepositoryFailureGitHub) ListLabeledIssues(ctx context.Context, repo string, labels []string) ([]github.Issue, error) {
	if err := g.Failures[repo]; err != nil {
		return nil, err
	}
	return g.FakeGitHub.ListLabeledIssues(ctx, repo, labels)
}

// failingPullRequests keeps the persisted run alive while its PR always
// returns the same conclusive GitHub error.
type FailingPullRequests struct {
	*FakeGitHub
	Err error
}

func (g *FailingPullRequests) GetPullRequest(context.Context, string, int) (*github.PullRequest, error) {
	return nil, g.Err
}
