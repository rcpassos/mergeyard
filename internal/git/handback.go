package git

import (
	"context"
	"errors"
)

// InspectHandback reconciles published ancestry while allowing manual commits
// and edits. It never adopts a remote head absent from the preserved local work.
func (m *Manager) InspectHandback(ctx context.Context, run Run, target string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	if target != "" && remote != target {
		return "", failure("handback.head_ambiguous", run.Path, errors.New("PR and remote head disagree"))
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.inspectReview(ctx, run); err != nil {
		return CommitResult{}, err
	}
	origin, err := verifyOrigin(ctx, run)
	if err != nil {
		return CommitResult{}, err
	}
	remote, err := optionalRemoteHead(ctx, run, origin)
	if err != nil {
		return CommitResult{}, err
	}
	if remote != previous {
		return CommitResult{}, failure("handback.head_ambiguous", run.Path, errors.New("remote changed before manual commit"))
	}
	ancestor := previous
	if ancestor == "" {
		ancestor = run.BaseSHA
	}
	if _, err := command(ctx, run.Path, "handback.head_diverged", "merge-base", "--is-ancestor", ancestor, "HEAD"); err != nil {
		return CommitResult{}, err
	}
	return commit(ctx, run, Phase{Title: "manual handback"})
}

// PushHandback accepts only the inspected remote or our journaled commit,
// including a genuinely absent branch before PR creation. Never force push.
func (m *Manager) PushHandback(ctx context.Context, run Run, previous, target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.inspectReview(ctx, run); err != nil {
		return err
	}
	if !shaPattern.MatchString(target) {
		return failure("handback.head_ambiguous", run.Path, errors.New("invalid manual commit"))
	}
	if err := inspectFixTarget(ctx, run, target); err != nil {
		return err
	}
	origin, err := verifyOrigin(ctx, run)
	if err != nil {
		return err
	}
	remote, err := optionalRemoteHead(ctx, run, origin)
	if err != nil {
		return err
	}
	if remote == target {
		return nil
	}
	if remote != previous {
		return failure("handback.head_ambiguous", run.Path, errors.New("remote changed during manual publication"))
	}
	return push(ctx, run, origin, target)
}
