package git

import (
	"context"
	"errors"
	"os"
)

// Cleanup removes a completed run's worktree, metadata, and local branch. The
// caller must confirm completion; stopped, manual, and needs-attention runs must
// retain their worktrees. Dirty worktrees are refused, and remote refs are kept.
// Repeating cleanup after an interrupted or completed cleanup is safe.
func (m *Manager) Cleanup(ctx context.Context, run Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validateRun(run); err != nil {
		return err
	}
	if _, err := os.Lstat(run.Path); err == nil {
		if err := verifyWorktree(ctx, run); err != nil {
			return err
		}
		if _, err := command(ctx, run.BasePath, "git.cleanup", "worktree", "remove", "--", run.Path); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return failure("git.cleanup", run.Path, err)
	}
	if _, err := command(ctx, run.BasePath, "git.cleanup", "worktree", "prune", "--expire", "now"); err != nil {
		return err
	}
	exists, err := refExists(ctx, run.BasePath, "refs/heads/"+run.Branch)
	if err != nil || !exists {
		return err
	}
	// A completed PR may have been squash-merged, so ancestry against the local
	// base is not a valid completion check. This deletes only the local branch.
	_, err = command(ctx, run.BasePath, "git.cleanup", "branch", "-D", "--", run.Branch)
	return err
}
