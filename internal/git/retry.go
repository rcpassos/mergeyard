package git

import (
	"context"
	"errors"
)

// InspectRetry observes ownership, edits, and the published branch without
// resetting, cleaning, committing, or switching the user's worktree.
func (m *Manager) InspectRetry(ctx context.Context, run Run, target string, allowEdits bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.inspectReview(ctx, run); err != nil {
		return "", err
	}
	head, err := command(ctx, run.Path, "git.ref", "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	dirty, err := command(ctx, run.Path, "git.status", "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return "", err
	}
	if dirty != "" && !allowEdits {
		return head, failure("retry.worktree_dirty", run.Path, errors.New("preserved edits must be committed and published or moved aside before reviewing or waiting for CI"))
	}
	if target == "" {
		return head, nil
	}
	if !shaPattern.MatchString(target) {
		return head, failure("retry.head_ambiguous", run.Path, errors.New("PR head is missing or invalid"))
	}
	origin, err := verifyOrigin(ctx, run)
	if err != nil {
		return head, err
	}
	remote, err := remoteHead(ctx, run, origin)
	if err != nil {
		return head, err
	}
	if remote != target {
		return head, failure("retry.head_ambiguous", run.Path, errors.New("Git remote and PR disagree; inspect before retrying"))
	}
	if head != target {
		if !allowEdits {
			return head, failure("retry.head_diverged", run.Path, errors.New("local head differs from published PR head; align preserved work before retrying"))
		}
		if _, err := command(ctx, run.Path, "retry.head_diverged", "merge-base", "--is-ancestor", target, "HEAD"); err != nil {
			return head, err
		}
	}
	return head, nil
}
