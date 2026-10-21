package github

import (
	"context"
	"path/filepath"
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
	// Worktree is the managed worktree directory the preparation created,
	// relative to the Mergeyard workspace. The issue may be public, so it never
	// carries the operator's absolute path.
	Worktree string
	// Reason explains why the leftovers were preserved.
	Reason string
	// Unverified reports that Mergeyard could not check the leftovers, so they
	// may hold work or may no longer exist.
	Unverified bool
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
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(leftovers.Branch) == "" || strings.TrimSpace(leftovers.Worktree) == "" {
		return codedError("github.invalid_input", "expected a run, a branch, and a worktree", nil)
	}
	if filepath.IsAbs(leftovers.Worktree) || strings.HasPrefix(leftovers.Worktree, "/") || strings.HasPrefix(leftovers.Worktree, "~") {
		return codedError("github.invalid_input", "expected a worktree relative to the Mergeyard workspace", nil)
	}
	login, err := c.authenticatedLogin(ctx)
	if err != nil {
		return err
	}
	marker := StopLeftoversMarker + ":" + runID
	prefix := marker + "\nMergeyard stopped run " + runID + " while it was preparing and kept the leftovers it could not remove:\n\n"
	reason := leftovers.Reason
	if leftovers.Unverified {
		reason = "The leftovers could not be verified: they may hold work, or may no longer exist. " + reason
	}
	body := prefix + "- Local branch `" + leftovers.Branch + "`\n- Worktree `" + leftovers.Worktree + "` in the Mergeyard workspace on the machine that ran it\n\n" + reason +
		"\n\nMergeyard did not delete them. Run detail shows the full worktree path. Remove the worktree and the branch before another run prepares this issue."
	_, err = c.ensureComment(ctx, repo, number, body, func(comment Comment) bool {
		return comment.User != nil && strings.EqualFold(comment.User.Login, login) && strings.HasPrefix(comment.Body, prefix)
	})
	return err
}
