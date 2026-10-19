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
	remoteCtx, remoteCancel := m.fetchDeadline(ctx)
	origin, publish, err := m.inspectHandbackPush(remoteCtx, run, previous, target)
	remoteCancel()
	if err != nil || !publish {
		return err
	}
	return m.pushWithinBudget(ctx, run, origin, target)
}

func (m *Manager) inspectHandbackPush(ctx context.Context, run Run, previous, target string) (string, bool, error) {
	if err := m.inspectReview(ctx, run); err != nil {
		return "", false, err
	}
	if !shaPattern.MatchString(target) {
		return "", false, failure("handback.head_ambiguous", run.Path, errors.New("invalid manual commit"))
	}
	if err := inspectFixTarget(ctx, run, target); err != nil {
		return "", false, err
	}
	origin, err := verifyOrigin(ctx, run)
	if err != nil {
		return "", false, err
	}
	remote, err := optionalRemoteHead(ctx, run, origin)
	if err != nil {
		return "", false, err
	}
	if remote == target {
		return origin, false, nil
	}
	if remote != previous {
		return "", false, failure("handback.head_ambiguous", run.Path, errors.New("remote changed during manual publication"))
	}
	return origin, true, nil
}
