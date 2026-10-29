package testkit

import (
	"context"

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/github"
)

type CiGitHub struct {
	*FakeGitHub
	Evidence      ci.Evidence
	QueryError    error
	ReadyError    error
	ReadyCalls    int
	ApplyReady    bool
	AfterReady    func()
	AfterEvidence func()
}

func (f *CiGitHub) PullRequestEvidence(_ context.Context, _ string, pr github.PullRequest) (ci.Evidence, error) {
	sha := pr.Head.SHA
	e := f.Evidence
	e.Checks = append([]ci.Check(nil), e.Checks...)
	e.SHA = sha
	for i := range e.Checks {
		if e.Checks[i].SHA == "" {
			e.Checks[i].SHA = sha
		}
	}
	if f.AfterEvidence != nil {
		f.AfterEvidence()
	}
	return e, f.QueryError
}
func (f *CiGitHub) MarkReady(_ context.Context, _ string, n int) error {
	f.ReadyCalls++
	if f.ApplyReady {
		for _, pr := range f.Prs {
			if pr.Number == n {
				pr.Draft = false
			}
		}
	}
	if f.AfterReady != nil {
		f.AfterReady()
	}
	return f.ReadyError
}

// Resolve the PR head from the managed branch as implementation/fixes publish.
type CiBranchGitHub struct{ *CiGitHub }

func (f CiBranchGitHub) GetPullRequest(ctx context.Context, repo string, n int) (*github.PullRequest, error) {
	pr, err := f.FakeGitHub.GetPullRequest(ctx, repo, n)
	if err == nil && pr != nil {
		pr.Head.SHA = f.Head(pr.Head.Ref)
	}
	return pr, err
}
