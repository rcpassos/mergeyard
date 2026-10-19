package git

import "context"

// CommitImplement pins local work without publishing it, so configured checks
// can gate the exact commit. Repeating after a commit-before-journal crash is safe.
func (m *Manager) CommitImplement(ctx context.Context, run Run, phase Phase) (CommitResult, error) {
	ctx, release, err := m.begin(ctx, m.timeouts.Worktree)
	if err != nil {
		return CommitResult{}, err
	}
	defer release()
	if err := m.validateRun(run); err != nil {
		return CommitResult{}, err
	}
	if err := verifyWorktree(ctx, run); err != nil {
		return CommitResult{}, err
	}
	if _, err := verifyOrigin(ctx, run); err != nil {
		return CommitResult{}, err
	}
	return commit(ctx, run, phase)
}

// PushImplement publishes only the commit which passed checks; edits after the
// check began require inspection instead of quietly changing checked code.
func (m *Manager) PushImplement(ctx context.Context, run Run, target string) error {
	ctx, release, err := m.begin(ctx, m.timeouts.publicationBudget())
	if err != nil {
		return err
	}
	defer release()
	// Local preflight, including the full status scan, keeps the worktree
	// budget; only the push itself uses the push budget.
	preflightCtx, preflightCancel := m.worktreeDeadline(ctx)
	origin, err := m.pushPreflight(preflightCtx, run, target)
	preflightCancel()
	if err != nil {
		return err
	}
	return m.pushWithinBudget(ctx, run, origin, target)
}
