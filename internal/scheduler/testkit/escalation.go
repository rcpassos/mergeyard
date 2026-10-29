package testkit

import (
	"context"
	"errors"

	"github.com/rcpassos/mergeyard/internal/escalation"
	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
)

type EscalationGitHub struct {
	*CiGitHub
	FailBody  bool
	BodyCode  string
	BodyCalls int
}

func (f *EscalationGitHub) UpdateEscalation(_ context.Context, _ string, number int, snapshot escalation.Snapshot) error {
	f.BodyCalls++
	if f.FailBody {
		if f.BodyCode != "" {
			return &fault.Error{Code: f.BodyCode, Message: "Publication rejected"}
		}
		return errors.New("GitHub unavailable")
	}
	for _, pr := range f.Prs {
		if pr.Number == number {
			body, err := github.UpdateEscalationBody(pr.Body, snapshot)
			pr.Body = body
			return err
		}
	}
	return errors.New("PR missing")
}

type EscalationGit struct {
	*managedgit.Manager
	Unavailable bool
	DiffCalls   int
	AfterDiff   func()
}

type DelayedVerificationGitHub struct {
	*EscalationGitHub
	FailVerification bool
}

func (f *DelayedVerificationGitHub) UpdatePullRequest(ctx context.Context, repo string, number int, content github.PullRequestContent) (*github.PullRequest, error) {
	if f.FailVerification {
		return nil, &fault.Error{Code: "github.unavailable", Message: "Verification body write unavailable"}
	}
	return f.FakeGitHub.UpdatePullRequest(ctx, repo, number, content)
}

func (g *EscalationGit) EscalationDiff(ctx context.Context, run managedgit.Run, head string) ([]escalation.FileChange, error) {
	g.DiffCalls++
	if g.AfterDiff != nil {
		defer g.AfterDiff()
	}
	if g.Unavailable {
		return nil, errors.New("diff unavailable")
	}
	return g.Manager.EscalationDiff(ctx, run, head)
}
