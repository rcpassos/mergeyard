package git

import "context"

// CommitImplement pins local work without publishing it, so configured checks
// can gate the exact commit. Repeating after a commit-before-journal crash is safe.
func (m *Manager) CommitImplement(ctx context.Context, run Run, phase Phase) (CommitResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := m.worktree(ctx)
	defer cancel()
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
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := m.remote(ctx)
	defer cancel()
	if err := m.validateRun(run); err != nil {
		return err
	}
	if !shaPattern.MatchString(target) {
		return failure("git.fix_target_invalid", run.Path, nil)
	}
	if err := verifyWorktree(ctx, run); err != nil {
		return err
	}
	origin, err := verifyOrigin(ctx, run)
	if err != nil {
		return err
	}
	if err := inspectFixTarget(ctx, run, target); err != nil {
		return err
	}
	return push(ctx, run, origin, target)
}
