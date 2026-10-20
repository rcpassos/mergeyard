package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// Leftovers names what an interrupted preparation left behind. A discarded
// preparation leaves nothing; preserved leftovers keep their branch and
// worktree for human attention.
type Leftovers struct {
	// Branch is the local branch the preparation created.
	Branch string
	// Path is the managed worktree directory the preparation created.
	Path string
	// Preserved reports that the leftovers remain for human attention.
	Preserved bool
	// Reason explains why the leftovers were preserved.
	Reason string
}

// DiscardPreparation releases the leftovers of an interrupted preparation, so
// the next run for the same issue can prepare. The preparation intent proves
// that the branch and the worktree belong to this run. They are removed only
// while they hold no work: the branch still at the recorded start, and a
// checkout that belongs to the run and is clean. Leftovers with commits or
// changes are preserved and reported, so the caller can name them for human
// attention.
//
// The returned error reports a failure to attempt the release, such as a
// cancelled caller or a Git failure. A leftover that cannot be removed is
// reported through the returned leftovers instead, because stopping the run
// must not depend on removing them.
func (m *Manager) DiscardPreparation(ctx context.Context, req PrepareRequest) (Leftovers, error) {
	ctx, release, err := m.begin(ctx, m.timeouts.Metadata+m.timeouts.Worktree)
	if err != nil {
		return Leftovers{}, err
	}
	defer release()
	run, err := m.newRun(req)
	if err != nil {
		return Leftovers{}, err
	}
	intent, found, err := readPrepareIntent(run)
	if err != nil {
		return Leftovers{}, err
	}
	if !found {
		// Without a claim this run created no branch or worktree that it
		// could own. Another run's leftovers are not this run's to remove.
		return Leftovers{}, nil
	}
	if !intent.identifies(run) {
		// The claim does not identify this run, so the leftovers it names
		// cannot be proven to belong to this run.
		return Leftovers{Branch: intent.Branch, Path: intent.Path, Preserved: true, Reason: "The preparation claim does not identify this run."}, nil
	}
	run.BaseSHA = intent.BaseSHA
	worktreeCtx, worktreeCancel := m.worktreeDeadline(ctx)
	defer worktreeCancel()
	preserved, err := discardPreparedWorktree(worktreeCtx, run, intent)
	if err != nil {
		return Leftovers{}, err
	}
	if preserved != "" {
		return Leftovers{Branch: intent.Branch, Path: intent.Path, Preserved: true, Reason: preserved}, nil
	}
	if err := removePrepareIntent(run); err != nil {
		return Leftovers{}, err
	}
	return Leftovers{}, nil
}

// discardPreparedWorktree removes the leftovers of an interrupted preparation
// once they are proven to hold no work. It returns the reason the leftovers
// were preserved, or an empty reason when they were removed. The branch must
// still sit at the recorded start, and the checkout must belong to this run
// and be clean.
func discardPreparedWorktree(ctx context.Context, run Run, intent prepareIntent) (string, error) {
	if err := verifyBase(ctx, run.BasePath); err != nil {
		return "", err
	}
	atStart, err := branchAtStart(ctx, run, intent.BaseSHA)
	if err != nil {
		return "", err
	}
	if !atStart {
		return "the prepared branch holds commits; preserved for inspection", nil
	}
	discardable, err := preparedWorktreeDiscardable(ctx, run)
	if err != nil {
		return "", err
	}
	if !discardable {
		return "the prepared worktree holds work that this run cannot claim; preserved for inspection", nil
	}
	return "", removePreparedWorktree(ctx, run)
}

// preparedWorktreeDiscardable reports whether the leftover worktree belongs to
// this run and holds no work, so it can be discarded. Ownership is proven the
// way an interrupted creation is recognized: an absent path, an empty
// directory, Git's own unfinished initialization, or a checkout on the run
// branch attached to the managed base. Such a checkout must also be clean,
// because a change or a commit is work that is never deleted.
func preparedWorktreeDiscardable(ctx context.Context, run Run) (bool, error) {
	owned, checkout, err := interruptedWorktreeOwnership(ctx, run)
	if err != nil {
		return false, err
	}
	if !owned {
		return false, nil
	}
	if !checkout {
		// An absent path, an empty directory, and Git's own unfinished
		// initialization hold no work.
		return true, nil
	}
	status, err := command(ctx, run.Path, "git.status", "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		if ctx.Err() != nil {
			return false, contextFailure(ctx, "git.worktree", run.Path, ctx.Err())
		}
		// A checkout whose state cannot be read is not proven to hold no
		// work, so the release fails and is retried instead of guessing.
		return false, err
	}
	return status == "", nil
}

// removePreparedWorktree deletes a discarded preparation's worktree and its
// local branch. The path is already proven to hold no work, so the removal is
// unconditional.
func removePreparedWorktree(ctx context.Context, run Run) error {
	if err := os.RemoveAll(run.Path); err != nil {
		return failure("git.worktree", run.Path, err)
	}
	// A killed 'worktree add' can leave an initialization lock, and prune
	// preserves locked metadata, which would keep the branch checked out.
	// Unlocking an unregistered or unlocked tree fails harmlessly; prune and
	// branch deletion below report any real problem.
	_, _ = command(ctx, run.BasePath, "git.worktree", "worktree", "unlock", "--", run.Path)
	if _, err := command(ctx, run.BasePath, "git.worktree", "worktree", "prune", "--expire", "now"); err != nil {
		return err
	}
	exists, err := refExists(ctx, run.BasePath, "refs/heads/"+run.Branch)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	_, err = command(ctx, run.BasePath, "git.worktree", "branch", "-D", "--", run.Branch)
	return err
}

// interruptedWorktreeOwnership reports whether a leftover path belongs to this
// run and whether it holds a readable checkout. A path that is absent or empty
// holds no checkout. A checkout on the run branch attached to the managed base
// belongs to this run, and so does Git's own unfinished initialization, which
// holds no readable checkout. Anything whose ownership cannot be established
// is preserved.
func interruptedWorktreeOwnership(ctx context.Context, run Run) (owned bool, checkout bool, err error) {
	if _, err := os.Lstat(run.Path); errors.Is(err, os.ErrNotExist) {
		return true, false, nil
	} else if err != nil {
		return false, false, failure("git.worktree", run.Path, err)
	}
	if _, err := os.Lstat(filepath.Join(run.Path, ".git")); errors.Is(err, os.ErrNotExist) {
		// Only an empty directory is provably unused before a checkout exists.
		entries, err := os.ReadDir(run.Path)
		if err != nil {
			return false, false, failure("git.worktree", run.Path, err)
		}
		return len(entries) == 0, false, nil
	} else if err != nil {
		return false, false, failure("git.worktree", run.Path, err)
	}
	branch, err := command(ctx, run.Path, "git.worktree", "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		if ctx.Err() != nil {
			return false, false, contextFailure(ctx, "git.worktree", run.Path, ctx.Err())
		}
		// Git writes the linked registration before HEAD, so a deadline here
		// leaves no readable checkout. Accept it only when the link, backlink,
		// and common directory point at this run's managed base.
		initializing, err := managedInitialization(run)
		return initializing, false, err
	}
	if branch != run.Branch {
		return false, true, nil
	}
	common, err := command(ctx, run.Path, "git.worktree", "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		if ctx.Err() != nil {
			return false, false, contextFailure(ctx, "git.worktree", run.Path, ctx.Err())
		}
		return false, true, nil
	}
	got, err := os.Stat(common)
	if errors.Is(err, os.ErrNotExist) {
		return false, true, nil
	} else if err != nil {
		return false, false, failure("git.worktree", run.Path, err)
	}
	want, err := os.Stat(filepath.Join(run.BasePath, ".git"))
	if err != nil {
		return false, false, failure("git.worktree", run.BasePath, err)
	}
	return os.SameFile(got, want), true, nil
}
