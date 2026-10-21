package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
// checkout that belongs to the run and holds no change and no ignored file,
// because a local secret or a build output is a person's data too. Leftovers
// with commits, changes, or ignored files are preserved and reported, as are
// leftovers whose managed base cannot be verified, so the caller can name them
// for human attention. A run without a claim returns at once, without waiting
// for another operation.
//
// The returned error reports a failure to attempt the release, such as a
// cancelled caller or a Git failure. A leftover that cannot be removed is
// reported through the returned leftovers instead, because stopping the run
// must not depend on removing them.
func (m *Manager) DiscardPreparation(ctx context.Context, req PrepareRequest) (Leftovers, error) {
	run, err := m.newRun(req)
	if err != nil {
		return Leftovers{}, err
	}
	// A run without a claim created nothing to release, so it must not wait
	// for another operation's lock. Only this run's own operations write its
	// claim, and they are serialized with this call by the caller.
	if _, err := os.Lstat(prepareIntentPath(run)); errors.Is(err, os.ErrNotExist) {
		return Leftovers{}, nil
	}
	ctx, release, err := m.begin(ctx, m.timeouts.Metadata+m.timeouts.Worktree)
	if err != nil {
		return Leftovers{}, err
	}
	defer release()
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
	preserved, err := m.discardPreparedWorktree(worktreeCtx, run, intent)
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
func (m *Manager) discardPreparedWorktree(ctx context.Context, run Run, intent prepareIntent) (string, error) {
	if err := verifyBase(ctx, run.BasePath); err != nil {
		if ctx.Err() != nil {
			return "", err
		}
		// The claim outlives the managed base, which a person may have fixed by
		// hand. Without the base the branch and the checkout cannot be proven
		// empty, and stopping the run must not depend on repairing it.
		return "the managed base cannot be verified; preserved for inspection", nil
	}
	atStart, err := branchAtStart(ctx, run, intent.BaseSHA)
	if err != nil {
		return "", err
	}
	if !atStart {
		return "the prepared branch holds commits; preserved for inspection", nil
	}
	discardable, err := m.preparedWorktreeDiscardable(ctx, run, intent)
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
// because a change, a commit, or an ignored file is work that is never
// deleted.
func (m *Manager) preparedWorktreeDiscardable(ctx context.Context, run Run, intent prepareIntent) (bool, error) {
	owned, state, err := interruptedWorktreeOwnership(ctx, run)
	if err != nil {
		return false, err
	}
	if !owned {
		return false, nil
	}
	switch state {
	case leftoverEmpty:
		// An absent path and an empty directory hold no work.
		return true, nil
	case leftoverInitializing:
		// Git writes the working tree before HEAD, so this directory can hold
		// a complete checkout, a partial one, or files a person added. Only
		// the recorded start, unchanged and without additions, is safe.
		return m.preparedDirectoryHoldsOnlyStart(ctx, run, intent.BaseSHA)
	default:
		// Ignored files are a person's work too: a build output or a local
		// secret never shows in the plain status, so they are listed explicitly.
		status, err := command(ctx, run.Path, "git.status", "status", "--porcelain", "--untracked-files=all", "--ignored")
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
}

// preparedDirectoryHoldsOnlyStart reports whether a directory that Git never
// finished initializing holds no work beyond the recorded start. Every file it
// holds must match the recorded tree, and it must hold no file the tree does
// not. The working tree is compared through a temporary index, so the base's
// own index and HEAD never affect the answer. A file the recorded tree holds
// and the directory does not is a partial checkout: nothing is lost by
// removing the directory.
func (m *Manager) preparedDirectoryHoldsOnlyStart(ctx context.Context, run Run, start string) (bool, error) {
	if !shaPattern.MatchString(start) {
		return false, nil
	}
	index, err := os.CreateTemp(m.root, ".prepare-index-*")
	if err != nil {
		return false, failure("git.worktree", run.Path, err)
	}
	path := index.Name()
	if err := index.Close(); err != nil {
		os.Remove(path)
		return false, failure("git.worktree", run.Path, err)
	}
	defer os.Remove(path)
	env := []string{"GIT_INDEX_FILE=" + path}
	if _, err := commandWithEnv(ctx, run.BasePath, "git.worktree", env, "read-tree", start); err != nil {
		return false, err
	}
	// The refresh compares the recorded tree with the directory and updates
	// the entries that match. It exits with 1 when a file differs, which is a
	// difference to report rather than a failure.
	if _, err := commandWithEnv(ctx, run.BasePath, "git.worktree", env, "--work-tree="+run.Path, "update-index", "--refresh"); err != nil {
		var exit *exec.ExitError
		if ctx.Err() != nil || !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return false, err
		}
	}
	diff, err := commandWithEnv(ctx, run.BasePath, "git.worktree", env, "--work-tree="+run.Path, "diff-files", "--name-status")
	if err != nil {
		return false, err
	}
	for _, record := range strings.Split(diff, "\n") {
		if record == "" {
			continue
		}
		status, _, ok := strings.Cut(record, "\t")
		if !ok || status != "D" {
			// A file the directory holds differs from the recorded tree, so it
			// holds work that the release must not discard.
			return false, nil
		}
	}
	// Ignored files are listed too, because an ignore rule says nothing about
	// whether a person's data may be deleted.
	untracked, err := commandWithEnv(ctx, run.BasePath, "git.worktree", env, "--work-tree="+run.Path, "ls-files", "-o")
	if err != nil {
		return false, err
	}
	if untracked != "" {
		return false, nil
	}
	return m.preparedSubmodulesHoldNothing(ctx, run, start)
}

// preparedSubmodulesHoldNothing reports whether every submodule of the
// recorded tree is empty in the directory. Git compares a submodule by the
// commit it points at and never by the files inside it, and it skips the
// subtree of an indexed submodule when it lists untracked files, so the other
// checks cannot see a person adding files to an uninitialized submodule.
func (m *Manager) preparedSubmodulesHoldNothing(ctx context.Context, run Run, start string) (bool, error) {
	out, err := command(ctx, run.BasePath, "git.ls_tree", "ls-tree", "-r", "-z", start)
	if err != nil {
		return false, err
	}
	for _, record := range strings.Split(out, "\x00") {
		if record == "" {
			continue
		}
		fields, path, ok := strings.Cut(record, "\t")
		if !ok {
			// A record the release cannot read is not proof of an empty
			// submodule.
			return false, nil
		}
		mode, _, ok := strings.Cut(fields, " ")
		if !ok || mode != "160000" {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(run.Path, path))
		if errors.Is(err, os.ErrNotExist) {
			// The checkout never created the submodule.
			continue
		}
		if err != nil {
			return false, failure("git.worktree", filepath.Join(run.Path, path), err)
		}
		if len(entries) > 0 {
			// The submodule holds files that no other check can verify.
			return false, nil
		}
	}
	return true, nil
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

// leftoverState classifies what a leftover path holds.
type leftoverState int

const (
	// leftoverEmpty means the path is absent or an empty directory.
	leftoverEmpty leftoverState = iota
	// leftoverCheckout means the path holds a readable checkout.
	leftoverCheckout
	// leftoverInitializing means Git never finished initializing the checkout.
	leftoverInitializing
	// leftoverForeign means the path holds something this run cannot claim.
	leftoverForeign
)

// interruptedWorktreeOwnership reports whether a leftover path belongs to this
// run and what it holds. A path that is absent or empty holds nothing. A
// checkout on the run branch attached to the managed base belongs to this run.
// Git's own unfinished initialization belongs to this run too, and holds no
// readable checkout yet. Anything whose ownership cannot be established is
// preserved.
func interruptedWorktreeOwnership(ctx context.Context, run Run) (owned bool, state leftoverState, err error) {
	if _, err := os.Lstat(run.Path); errors.Is(err, os.ErrNotExist) {
		return true, leftoverEmpty, nil
	} else if err != nil {
		return false, leftoverForeign, failure("git.worktree", run.Path, err)
	}
	if _, err := os.Lstat(filepath.Join(run.Path, ".git")); errors.Is(err, os.ErrNotExist) {
		// Only an empty directory is provably unused before a checkout exists.
		entries, err := os.ReadDir(run.Path)
		if err != nil {
			return false, leftoverForeign, failure("git.worktree", run.Path, err)
		}
		if len(entries) == 0 {
			return true, leftoverEmpty, nil
		}
		return false, leftoverForeign, nil
	} else if err != nil {
		return false, leftoverForeign, failure("git.worktree", run.Path, err)
	}
	branch, err := command(ctx, run.Path, "git.worktree", "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		if ctx.Err() != nil {
			return false, leftoverForeign, contextFailure(ctx, "git.worktree", run.Path, ctx.Err())
		}
		// Git writes the linked registration before HEAD, so a deadline here
		// leaves no readable checkout. Accept it only when the link, backlink,
		// and common directory point at this run's managed base.
		initializing, err := managedInitialization(run)
		return initializing, leftoverInitializing, err
	}
	if branch != run.Branch {
		return false, leftoverCheckout, nil
	}
	common, err := command(ctx, run.Path, "git.worktree", "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		if ctx.Err() != nil {
			return false, leftoverForeign, contextFailure(ctx, "git.worktree", run.Path, ctx.Err())
		}
		return false, leftoverCheckout, nil
	}
	got, err := os.Stat(common)
	if errors.Is(err, os.ErrNotExist) {
		return false, leftoverCheckout, nil
	} else if err != nil {
		return false, leftoverForeign, failure("git.worktree", run.Path, err)
	}
	want, err := os.Stat(filepath.Join(run.BasePath, ".git"))
	if err != nil {
		return false, leftoverForeign, failure("git.worktree", run.BasePath, err)
	}
	return os.SameFile(got, want), leftoverCheckout, nil
}
