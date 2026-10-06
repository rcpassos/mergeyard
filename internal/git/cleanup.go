package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
)

// Cleanup removes a completed run's worktree, metadata, and local branch. The
// caller must confirm completion; stopped, manual, and needs-attention runs must
// retain their worktrees. Dirty worktrees and unpublished local commits are preserved; remote refs are kept.
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
	if err := cleanupPublished(ctx, run); err != nil {
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

// CleanupBranch deletes a completed run's published local ref; remote refs are retained.
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
	if err := cleanupPublished(ctx, run); err != nil {
		return err
	}
	// A completed PR may have been squash-merged, so ancestry against the local
	// base is not a valid completion check. This deletes only the local branch.
	_, err = command(ctx, run.BasePath, "git.cleanup", "branch", "-D", "--", run.Branch)
	return err
}

// Clean files do not imply published commits. Check the ref before removing its
// tree and again before deleting it, including after interrupted cleanup.
func cleanupPublished(ctx context.Context, run Run) error {
	exists, err := refExists(ctx, run.BasePath, "refs/heads/"+run.Branch)
	if err != nil || !exists {
		return err
	}
	head, err := command(ctx, run.BasePath, "git.ref", "rev-parse", "refs/heads/"+run.Branch)
	if err != nil {
		return err
	}
	// Preparation pinned this commit from the fetched public base.
	if head == run.BaseSHA {
		return nil
	}
	if shaPattern.MatchString(run.PublishedSHA) {
		contained, err := publishedContains(ctx, run, head, run.PublishedSHA)
		if err == nil && contained {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	origin, err := verifyOrigin(ctx, run)
	if err != nil {
		return err
	}
	out, err := command(ctx, run.BasePath, "git.cleanup_publication", "ls-remote", "--heads", "--", origin, "refs/heads/"+run.Branch)
	if err != nil {
		return err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return failure("git.unpublished_work", run.Path, errors.New("local commits have no confirmed published head; worktree and branch are preserved for human attention"))
	}
	if len(fields) != 2 || !shaPattern.MatchString(fields[0]) || fields[1] != "refs/heads/"+run.Branch {
		return failure("git.cleanup_publication", run.Path, errors.New("remote branch publication is ambiguous"))
	}
	published := fields[0]
	if head == published {
		return nil
	}
	if _, err := command(ctx, run.BasePath, "git.cleanup_publication", "cat-file", "-e", published+"^{commit}"); err != nil {
		if _, err := command(ctx, run.BasePath, "git.cleanup_publication", "fetch", "--no-tags", "--", origin, published); err != nil {
			return err
		}
	}
	contained, err := publishedContains(ctx, run, head, published)
	if err != nil {
		return err
	}
	if !contained {
		return failure("git.unpublished_work", run.Path, errors.New("local commits are not contained in the published head; worktree and branch are preserved for human attention"))
	}
	return nil
}
func publishedContains(ctx context.Context, run Run, head, published string) (bool, error) {
	_, err := command(ctx, run.BasePath, "git.cleanup_publication", "merge-base", "--is-ancestor", head, published)
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if ctx.Err() == nil && errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}
