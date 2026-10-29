package testkit

import (
	"context"

	"github.com/rcpassos/mergeyard/internal/github"
)

// Scope polling to the explicitly supplied test issue. All other operations use
// real GitHub, Git, tmux, SQLite, and the native Claude structured-output adapter.
type TestIssueGitHub struct {
	*github.Client
	Number int
}

func (c TestIssueGitHub) ListLabeledIssues(ctx context.Context, repo string, labels []string) ([]github.Issue, error) {
	issues, err := c.Client.ListLabeledIssues(ctx, repo, labels)
	if err != nil {
		return nil, err
	}
	for _, issue := range issues {
		if issue.Number == c.Number {
			return []github.Issue{issue}, nil
		}
	}
	return nil, nil
}
