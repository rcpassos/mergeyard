package git

import (
	"context"
	"errors"
)

// InspectHandback reconciles published ancestry while allowing manual commits
// and edits. It never adopts a remote head absent from the preserved local work.
func (m *Manager) InspectHandback(ctx context.Context, run Run, target string) (string, error) {
	ctx, release, err := m.begin(ctx, m.timeouts.Fetch)
	if err != nil {
		return "", err
	}
	defer release()
	remote, err := m.inspectHandbackAncestry(ctx, run)
	if err != nil {
		return "", err
	}
	if target != "" && remote != target {
		return "", failure("handback.head_ambiguous", run.Path, errors.New("PR and remote head disagree"))
	}
	return remote, nil
}

// The caller holds m.mu across inspection and any following commit.
func (m *Manager) inspectHandbackAncestry(ctx context.Context, run Run) (string, error) {
	if err := m.inspectReview(ctx, run); err != nil {
		return "", err
	}
	origin, err := verifyOrigin(ctx, run)
	if err != nil {
		return "", err
	}
	remote, err := optionalRemoteHead(ctx, run, origin)
	if err != nil {
		return "", err
	}
	ancestor := remote
	if ancestor == "" {
		ancestor = run.BaseSHA
	}
	if _, err := command(ctx, run.Path, "handback.head_diverged", "merge-base", "--is-ancestor", ancestor, "HEAD"); err != nil {
		return "", err
	}
	return remote, nil
}

// CommitHandback is replayable after committing but before saving the new SHA.
func (m *Manager) CommitHandback(ctx context.Context, run Run, previous string) (CommitResult, error) {
	// Publication covers the remote ancestry read and the manual commit, each
	// with its own class deadline.
	ctx, release, err := m.begin(ctx, m.timeouts.publicationBudget())
	if err != nil {
		return CommitResult{}, err
	}
	defer release()
	remoteCtx, remoteCancel := m.fetchDeadline(ctx)
	remote, err := m.inspectHandbackAncestry(remoteCtx, run)
	remoteCancel()
	if err != nil {
		return CommitResult{}, err
	}
	if remote != previous {
		return CommitResult{}, failure("handback.head_ambiguous", run.Path, errors.New("remote changed before manual commit"))
	}
	commitCtx, commitCancel := m.worktreeDeadline(ctx)
	result, err := commit(commitCtx, run, Phase{Title: "manual handback"})
	commitCancel()
	return result, err
}

// PushHandback accepts only the inspected remote or our journaled commit,
// including a genuinely absent branch before PR creation. Never force push.
func (m *Manager) PushHandback(ctx context.Context, run Run, previous, target string) error {
	ctx, release, err := m.begin(ctx, m.timeouts.publicationBudget())
	if err != nil {
		return err
	}
	defer release()
	// Local preflight keeps the worktree budget; the remote read and the push
	// keep their own class deadlines.
	preflightCtx, preflightCancel := m.worktreeDeadline(ctx)
	origin, err := m.handbackPreflight(preflightCtx, run, target)
	preflightCancel()
	if err != nil {
		return err
	}
	remoteCtx, remoteCancel := m.fetchDeadline(ctx)
	remote, err := optionalRemoteHead(remoteCtx, run, origin)
	remoteCancel()
	if err != nil {
		return err
	}
	if remote == target {
		return nil
	}
	if remote != previous {
		return failure("handback.head_ambiguous", run.Path, errors.New("remote changed during manual publication"))
	}
	return m.pushWithinBudget(ctx, run, origin, target)
}

// handbackPreflight verifies the preserved work and target commit under the
// caller's worktree deadline before the remote is observed.
func (m *Manager) handbackPreflight(ctx context.Context, run Run, target string) (string, error) {
	if err := m.inspectReview(ctx, run); err != nil {
		return "", err
	}
	if !shaPattern.MatchString(target) {
		return "", failure("handback.head_ambiguous", run.Path, errors.New("invalid manual commit"))
	}
	if err := inspectFixTarget(ctx, run, target); err != nil {
		return "", err
	}
	return verifyOrigin(ctx, run)
}
