package github

import (
	"context"
	"strings"
)

// StopLeftoversMarker identifies the comment that explains the leftovers an
// interrupted preparation left behind.
const StopLeftoversMarker = "<!-- mergeyard:stop:leftovers -->"

// EnsureStopLeftoversComment explains the leftovers a stopped preparation left
// behind, once per run, so the issue that needs attention names the branch and
// the worktree the user has to remove. The marker identifies the run, so a
// replay after an ambiguous write finds the existing comment instead of
// posting a second one.
func (c *Client) EnsureStopLeftoversComment(ctx context.Context, repo string, number int, runID, branch, path, reason string) error {
	if number <= 0 {
		return codedError("github.invalid_input", "expected a positive issue number", nil)
	}
	if _, err := issuePath(repo, number); err != nil {
		return err
	}
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(branch) == "" || strings.TrimSpace(path) == "" {
		return codedError("github.invalid_input", "expected a run, a branch, and a worktree path", nil)
	}
	login, err := c.authenticatedLogin(ctx)
	if err != nil {
		return err
	}
	marker := StopLeftoversMarker + ":" + runID
	prefix := marker + "\nMergeyard stopped run " + runID + " while it was preparing and kept the leftovers it could not remove:\n\n"
	body := prefix + "- Local branch `" + branch + "`\n- Worktree `" + path + "`\n\n" + reason +
		"\n\nMergeyard did not delete them. Remove the worktree and the branch before another run prepares this issue."
	_, err = c.ensureComment(ctx, repo, number, body, func(comment Comment) bool {
		return comment.User != nil && strings.EqualFold(comment.User.Login, login) && strings.HasPrefix(comment.Body, prefix)
	})
	return err
}
