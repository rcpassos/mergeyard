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
	if err := m.cleanupWorktree(ctx, run); err != nil {
		return err
	}
	return m.cleanupBranch(ctx, run)
}

// CleanupWorktree removes only a clean owned tree and prunes its registration.
// A missing directory is accepted only by callers with recorded removal intent.
func (m *Manager) CleanupWorktree(ctx context.Context, run Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cleanupWorktree(ctx, run)
}
func (m *Manager) cleanupWorktree(ctx context.Context, run Run) error {
	if err := m.validateRun(run); err != nil {
		return err
	}
	if err := verifyBase(ctx, run.BasePath); err != nil {
		return err
	}
	if _, err := verifyOrigin(ctx, run); err != nil {
		return err
	}
	if _, err := os.Lstat(run.Path); err == nil {
		if err := verifyWorktree(ctx, run); err != nil {
			return err
		}
		status, err := command(ctx, run.Path, "git.cleanup", "status", "--porcelain", "--untracked-files=all")
		if err != nil {
			return err
		}
		if status != "" {
			return failure("git.cleanup", run.Path, errors.New("dirty work is preserved for human attention"))
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
	return nil
}

// CleanupBranch deletes the completed run's local ref; remote refs are retained.
func (m *Manager) CleanupBranch(ctx context.Context, run Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cleanupBranch(ctx, run)
}
func (m *Manager) cleanupBranch(ctx context.Context, run Run) error {
	if err := m.validateRun(run); err != nil {
		return err
	}
	if err := verifyBase(ctx, run.BasePath); err != nil {
		return err
	}
	if _, err := verifyOrigin(ctx, run); err != nil {
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
