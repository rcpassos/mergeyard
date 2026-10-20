package github

import (
	"context"
	"strings"
)

// StopLeftoversMarker identifies the comment that explains the leftovers an
// interrupted preparation left behind.
const StopLeftoversMarker = "<!-- mergeyard:stop:leftovers -->"

// StoppedLeftovers names the leftovers a stopped preparation left behind, so
// the issue can tell the user what to remove.
type StoppedLeftovers struct {
	// Branch is the local branch the preparation created.
	Branch string
	// Path is the managed worktree directory the preparation created.
	Path string
	// Reason explains why the leftovers were preserved.
	Reason string
}

// EnsureStopLeftoversComment explains the leftovers a stopped preparation left
// behind, once per run, so the issue that needs attention names the branch and
// the worktree the user has to remove. The marker identifies the run, so a
// replay after an ambiguous write finds the existing comment instead of
// posting a second one.
func (c *Client) EnsureStopLeftoversComment(ctx context.Context, repo string, number int, runID string, leftovers StoppedLeftovers) error {
	if number <= 0 {
		return codedError("github.invalid_input", "expected a positive issue number", nil)
	}
	if _, err := issuePath(repo, number); err != nil {
		return err
	}
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(leftovers.Branch) == "" || strings.TrimSpace(leftovers.Path) == "" {
		return codedError("github.invalid_input", "expected a run, a branch, and a worktree path", nil)
	}
	login, err := c.authenticatedLogin(ctx)
	if err != nil {
		return err
	}
	marker := StopLeftoversMarker + ":" + runID
	prefix := marker + "\nMergeyard stopped run " + runID + " while it was preparing and kept the leftovers it could not remove:\n\n"
	body := prefix + "- Local branch `" + leftovers.Branch + "`\n- Worktree `" + leftovers.Path + "`\n\n" + leftovers.Reason +
		"\n\nMergeyard did not delete them. Remove the worktree and the branch before another run prepares this issue."
	_, err = c.ensureComment(ctx, repo, number, body, func(comment Comment) bool {
		return comment.User != nil && strings.EqualFold(comment.User.Login, login) && strings.HasPrefix(comment.Body, prefix)
	})
	return err
}
