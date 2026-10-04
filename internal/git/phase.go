package git

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// CommitAndPush stages tracked changes and non-ignored new files, keeps agent
// commits, and pushes only the run branch without force. RequireChanges compares
// the resulting tree with the base pinned when the run was first prepared.
func (m *Manager) CommitAndPush(ctx context.Context, run Run, phase Phase) (CommitResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validateRun(run); err != nil {
		return CommitResult{}, err
	}
	if err := verifyWorktree(ctx, run); err != nil {
		return CommitResult{}, err
	}
	origin, err := verifyOrigin(ctx, run)
	if err != nil {
		return CommitResult{}, err
	}
	if _, err := command(ctx, run.Path, "git.stage", "add", "--all", "--", "."); err != nil {
		return CommitResult{}, err
	}
	changed, err := hasDiff(ctx, run.Path, "diff", "--cached", "--quiet", "--exit-code", "--")
	if err != nil {
		return CommitResult{}, err
	}
	if changed {
		message := fmt.Sprintf("mergeyard: #%d %s", run.IssueNumber, phase.Title)
		if _, err := command(ctx, run.Path, "git.commit", "commit", "-m", message); err != nil {
			return CommitResult{}, err
		}
	}
	sha, err := command(ctx, run.Path, "git.ref", "rev-parse", "HEAD")
	if err != nil {
		return CommitResult{}, err
	}
	result := CommitResult{SHA: sha, Committed: changed}
	if phase.RequireChanges {
		different, err := hasDiff(ctx, run.Path, "diff", "--quiet", "--exit-code", run.BaseSHA, "HEAD", "--")
		if err != nil {
			return result, err
		}
		if !different {
			return result, failure("git.no_changes", run.Path, errors.New("run has no diff from its original base"))
		}
	}
	// An explicit verified fetch URL avoids an unrelated configured pushurl.
	// Explicit refspecs and options prevent mirror, tag, or forced updates.
	output, err := command(ctx, run.Path, "git.push", "-c", "remote.origin.mirror=false", "push", "--porcelain", "--no-force", "--no-follow-tags", "--recurse-submodules=no", "--", origin, "HEAD:refs/heads/"+run.Branch)
	if err != nil {
		for _, line := range strings.Split(output, "\n") {
			if strings.HasPrefix(line, "!\t") {
				return result, failure("git.push_rejected", run.Path, err)
			}
		}
		return result, err
	}
	return result, nil
}
