package github

import (
	"context"
	"strings"
)

const HarnessOverrideMarker = "<!-- mergeyard:harness-override:invalid -->"

// EnsureHarnessOverrideComment explains invalid labels once per issue, using
// the authenticated author's marker to recover ambiguous writes and restarts.
func (c *Client) EnsureHarnessOverrideComment(ctx context.Context, repo string, number int, reason string) error {
	if number <= 0 {
		return codedError("github.invalid_input", "expected a positive issue number", nil)
	}
	if _, err := issuePath(repo, number); err != nil {
		return err
	}
	login, err := c.authenticatedLogin(ctx)
	if err != nil {
		return err
	}
	prefix := HarnessOverrideMarker + "\nMergeyard could not claim this issue: "
	_, err = c.ensureComment(ctx, repo, number, prefix+reason, func(comment Comment) bool {
		return comment.User != nil && strings.EqualFold(comment.User.Login, login) && strings.HasPrefix(comment.Body, prefix)
	})
	return err
}
