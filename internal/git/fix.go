package git

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// CommitFix is replayable after a crash: already-created implementer or
// Mergeyard commits are retained, and a clean index never creates a new commit.
func (m *Manager) CommitFix(ctx context.Context, run Run, phase Phase, target string) (CommitResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validateRun(run); err != nil {
		return CommitResult{}, err
	}
	if !shaPattern.MatchString(target) {
		return CommitResult{}, failure("git.fix_target_invalid", run.Path, errors.New("invalid fix target"))
	}
	if err := verifyWorktree(ctx, run); err != nil {
		return CommitResult{}, err
	}
	origin, err := verifyOrigin(ctx, run)
	if err != nil {
		return CommitResult{}, err
	}
	remote, err := remoteHead(ctx, run, origin)
	if err != nil {
		return CommitResult{}, err
	}
	if remote != target {
		return CommitResult{}, failure("review.head_changed", run.Path, errors.New("remote head changed before fix commit"))
	}
	if _, err := command(ctx, run.Path, "git.fix_diverged", "merge-base", "--is-ancestor", target, "HEAD"); err != nil {
		return CommitResult{}, failure("git.fix_diverged", run.Path, err)
	}
	return commit(ctx, run, phase)
}

// PushFix accepts either the original target or the journaled new commit on the
// remote, covering a crash after a successful push. Any other head needs attention.
func (m *Manager) PushFix(ctx context.Context, run Run, previous, target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validateRun(run); err != nil {
		return err
	}
	if !shaPattern.MatchString(previous) || !shaPattern.MatchString(target) {
		return failure("git.fix_target_invalid", run.Path, errors.New("invalid fix target"))
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
	remote, err := remoteHead(ctx, run, origin)
	if err != nil {
		return err
	}
	if remote != previous && remote != target {
		return failure("review.head_changed", run.Path, errors.New("remote head changed during fix publication"))
	}
	if remote == target {
		return nil
	}
	return push(ctx, run, origin, target)
}

// InspectFixTarget prevents a re-review from testing unpublished edits after
// publication. Ignored build output remains allowed; all other work is preserved.
func (m *Manager) InspectFixTarget(ctx context.Context, run Run, target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validateRun(run); err != nil {
		return err
	}
	if !shaPattern.MatchString(target) {
		return failure("git.fix_target_invalid", run.Path, errors.New("invalid fix target"))
	}
	if err := verifyWorktree(ctx, run); err != nil {
		return err
	}
	if _, err := verifyOrigin(ctx, run); err != nil {
		return err
	}
	return inspectFixTarget(ctx, run, target)
}
func inspectFixTarget(ctx context.Context, run Run, target string) error {
	head, err := command(ctx, run.Path, "git.ref", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	dirty, err := command(ctx, run.Path, "git.status", "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return err
	}
	if head != target || dirty != "" {
		return failure("review.head_changed", run.Path, errors.New("workspace changed after fix commit was pinned"))
	}
	return nil
}

func remoteHead(ctx context.Context, run Run, origin string) (string, error) {
	out, err := command(ctx, run.Path, "git.remote_head", "ls-remote", "--heads", "--", origin, "refs/heads/"+run.Branch)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(out)
	if len(fields) != 2 || !shaPattern.MatchString(fields[0]) || fields[1] != "refs/heads/"+run.Branch {
		return "", failure("review.head_changed", run.Path, fmt.Errorf("run branch missing or ambiguous on remote"))
	}
	return fields[0], nil
}
