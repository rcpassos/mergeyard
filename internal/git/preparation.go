package git

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rcpassos/mergeyard/internal/fault"
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
	// Unverified reports that the release could not check the leftovers, so
	// they may hold work or may no longer exist.
	Unverified bool
}

// DiscardPreparation releases the leftovers of an interrupted preparation, so
// the next run for the same issue can prepare. The preparation intent proves
// that the branch and the worktree belong to this run. They are removed only
// while they hold no work: the branch still at the recorded start, and a
// checkout that belongs to the run and holds nothing beyond the recorded start,
// not even an ignored file, because a local secret or a build output is a
// person's data too. Leftovers with commits, changes, or ignored files are
// preserved and reported, so the caller can name them for human attention. A
// run without a claim returns at once, without waiting for another operation.
//
// Leftovers the release cannot check are preserved and reported as
// unverified: a claim that cannot be read or does not identify this run, a
// managed base that is missing or replaced, a checkout this run cannot claim,
// or a Git check that fails or exceeds its deadline. Stopping the run must not
// depend on any of them, because a retry may never clear them.
//
// The returned error reports only that the release was not attempted: invalid
// input, or a caller that gave up before the check finished, so the caller can
// try again.
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
	leftovers, err := m.releaseClaimedLeftovers(ctx, run)
	if err == nil {
		return leftovers, nil
	}
	if ctx.Err() != nil {
		return Leftovers{}, err
	}
	// The check or the removal failed on its own, not because the caller gave
	// up. A removal that failed partway may have deleted part of leftovers
	// already proven empty, never anything else. The leftovers are named the
	// way preparation names them for this run.
	reason := "Git could not check or remove them; preserved for inspection"
	var failed *fault.Error
	if errors.As(err, &failed) && failed.Code != "" {
		reason = "Git could not check or remove them (" + failed.Code + "); preserved for inspection"
	}
	return unverifiedLeftovers(run, prepareIntent{}, reason), nil
}

// releaseClaimedLeftovers releases the leftovers named by this run's claim
// while holding the manager's lock. Its errors are the failures
// DiscardPreparation turns into unverified leftovers.
func (m *Manager) releaseClaimedLeftovers(ctx context.Context, run Run) (Leftovers, error) {
	ctx, release, err := m.begin(ctx, m.timeouts.Metadata+m.timeouts.Worktree)
	if err != nil {
		return Leftovers{}, err
	}
	defer release()
	intent, found, err := readPrepareIntent(run)
	if err != nil {
		// A crash or a person can leave the claim empty or corrupt. It no
		// longer proves anything, so the leftovers it guarded stay.
		return unverifiedLeftovers(run, prepareIntent{}, "the preparation claim cannot be read; preserved for inspection"), nil
	}
	if !found {
		// Without a claim this run created no branch or worktree that it
		// could own. Another run's leftovers are not this run's to remove.
		return Leftovers{}, nil
	}
	if !intent.identifies(run) {
		// The claim does not identify this run, so the leftovers it names
		// cannot be proven to belong to this run.
		return unverifiedLeftovers(run, intent, "the preparation claim does not identify this run; preserved for inspection"), nil
	}
	run.BaseSHA = intent.BaseSHA
	worktreeCtx, worktreeCancel := m.worktreeDeadline(ctx)
	defer worktreeCancel()
	preserved, err := m.discardPreparedWorktree(worktreeCtx, run, intent)
	if err != nil {
		return Leftovers{}, err
	}
	if preserved.reason != "" {
		return Leftovers{Branch: intent.Branch, Path: intent.Path, Preserved: true, Reason: preserved.reason, Unverified: preserved.unverified}, nil
	}
	if err := removePrepareIntent(run); err != nil {
		return Leftovers{}, err
	}
	return Leftovers{}, nil
}

// unverifiedLeftovers reports leftovers the release could not check. They are
// named by the claim where it names them, and otherwise by the branch and the
// worktree preparation creates for this run.
func unverifiedLeftovers(run Run, intent prepareIntent, reason string) Leftovers {
	leftovers := Leftovers{Branch: intent.Branch, Path: intent.Path, Preserved: true, Unverified: true, Reason: reason}
	if leftovers.Branch == "" {
		leftovers.Branch = run.Branch
	}
	if leftovers.Path == "" {
		leftovers.Path = run.Path
	}
	return leftovers
}

// preservation explains why the leftovers of an interrupted preparation stay.
// An empty reason means they were removed.
type preservation struct {
	reason string
	// unverified reports that the leftovers could not be checked, as opposed
	// to holding work that the check found.
	unverified bool
}

// discardPreparedWorktree removes the leftovers of an interrupted preparation
// once they are proven to hold no work, and otherwise explains why they stay.
// The branch must still sit at the recorded start, and the checkout must
// belong to this run and hold nothing beyond the recorded start. Prepare's
// recovery of an interrupted creation applies the same check.
func (m *Manager) discardPreparedWorktree(ctx context.Context, run Run, intent prepareIntent) (preservation, error) {
	if err := verifyBase(ctx, run.BasePath); err != nil {
		if ctx.Err() != nil {
			return preservation{}, err
		}
		// The claim outlives the managed base, which a person may have fixed by
		// hand. Without the base the branch and the checkout cannot be proven
		// empty, and stopping the run must not depend on repairing it.
		return preservation{reason: "the managed base cannot be verified; preserved for inspection", unverified: true}, nil
	}
	atStart, err := branchAtStart(ctx, run, intent.BaseSHA)
	if err != nil {
		return preservation{}, err
	}
	if !atStart {
		return preservation{reason: "the prepared branch holds commits; preserved for inspection"}, nil
	}
	owned, state, err := interruptedWorktreeOwnership(ctx, run)
	if err != nil {
		return preservation{}, err
	}
	if state == leftoverDetached {
		// A checkout that is not attached to the managed base, such as one
		// whose base was replaced, cannot be checked for work.
		return preservation{reason: "the prepared worktree is not attached to the managed base; preserved for inspection", unverified: true}, nil
	}
	if !owned {
		return preservation{reason: "the prepared worktree holds work that this run cannot claim; preserved for inspection"}, nil
	}
	empty, err := m.preparedWorktreeHoldsNothing(ctx, run, intent, state)
	if err != nil {
		return preservation{}, err
	}
	if !empty {
		return preservation{reason: "the prepared worktree holds work; preserved for inspection"}, nil
	}
	return preservation{}, removePreparedWorktree(ctx, run)
}

// preparedWorktreeHoldsNothing reports whether a leftover worktree that
// belongs to this run holds no work, so it can be discarded. State is what
// interruptedWorktreeOwnership found at the path. A checkout Git never
// finished, or still holds locked for initialization, is compared with the
// recorded start, because a killed creation leaves it partly written. Any other
// checkout must also be clean, because a change, a commit, or an ignored file
// is work that is never deleted.
func (m *Manager) preparedWorktreeHoldsNothing(ctx context.Context, run Run, intent prepareIntent, state leftoverState) (bool, error) {
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
		initializing, err := checkoutLockedForInitialization(run)
		if err != nil {
			return false, err
		}
		if initializing {
			// A checkout cut short after HEAD was written reads as one whose
			// files are missing or untracked, so its status proves nothing.
			// Compare it with the recorded start like an unfinished one.
			return m.preparedDirectoryHoldsOnlyStart(ctx, run, intent.BaseSHA)
		}
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
	diff, err := commandWithEnv(ctx, run.BasePath, "git.worktree", env, "--work-tree="+run.Path, "diff-files", "--name-status", "-z")
	if err != nil {
		return false, err
	}
	records := strings.Split(diff, "\x00")
	for i := 0; i+1 < len(records); i += 2 {
		status, path := records[i], records[i+1]
		switch status {
		case "D":
			// Never written before the creation stopped.
			continue
		case "M":
			// A killed checkout leaves the file it was writing cut short, and a
			// cut-short file is a start of its recorded contents.
			partial, err := preparedFileIsStartOfRecorded(ctx, run, start, path)
			if err != nil {
				return false, err
			}
			if partial {
				continue
			}
		}
		// A file the directory holds differs from the recorded tree, so it
		// holds work that the release must not discard.
		return false, nil
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

// preparedFileIsStartOfRecorded reports whether a regular file in the
// directory holds only the beginning of its recorded contents, empty included.
// Git writes a file by opening it and then streaming the contents, so a creation
// killed midway leaves such a file. Git writes some files converted, such as
// line endings or an ident expansion, so the beginning may also be of the
// converted contents. A file that holds anything else is work.
func preparedFileIsStartOfRecorded(ctx context.Context, run Run, start, path string) (bool, error) {
	file := filepath.Join(run.Path, path)
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() {
		return false, nil
	}
	held, err := os.ReadFile(file)
	if err != nil {
		return false, failure("git.worktree", file, err)
	}
	// One byte past the file's size is all of the recorded contents the
	// comparison needs, so a large recorded file is never held whole.
	limit := int(info.Size()) + 1
	for _, args := range [][]string{
		{"cat-file", "blob", start + ":" + path},
		// The conversion follows the attributes of the recorded start, because
		// the directory's own .gitattributes may not be written yet. It runs any
		// filter the repository configures, as the checkout itself did.
		{"--attr-source=" + start, "cat-file", "--filters", start + ":" + path},
	} {
		recorded, err := commandPrefix(ctx, run.BasePath, "git.worktree", nil, limit, args...)
		if err != nil {
			if ctx.Err() != nil {
				return false, err
			}
			// A recorded entry that is not a readable blob, such as a submodule,
			// or a Git that cannot convert, proves nothing about the file.
			continue
		}
		if bytes.HasPrefix(recorded, held) {
			return true, nil
		}
	}
	return false, nil
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

// checkoutLockedForInitialization reports whether Git still holds the
// checkout's registration locked for initialization, which is how a killed
// 'worktree add' leaves a checkout that wrote HEAD but only some of its files.
// The checkout is already proven to be attached to the managed base.
func checkoutLockedForInitialization(run Run) (bool, error) {
	link, err := os.ReadFile(filepath.Join(run.Path, ".git"))
	if err != nil {
		return false, failure("git.worktree", run.Path, err)
	}
	admin, ok := gitDirTarget(string(link), run.Path)
	if !ok {
		return false, nil
	}
	return initializationLocked(admin)
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
	// leftoverDetached means the path holds a checkout that is not attached to
	// the managed base, such as one whose base was replaced, so its contents
	// cannot be checked.
	leftoverDetached
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
		if err == nil && !initializing {
			return false, leftoverDetached, nil
		}
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
		return false, leftoverDetached, nil
	}
	got, err := os.Stat(common)
	if errors.Is(err, os.ErrNotExist) {
		return false, leftoverDetached, nil
	} else if err != nil {
		return false, leftoverForeign, failure("git.worktree", run.Path, err)
	}
	want, err := os.Stat(filepath.Join(run.BasePath, ".git"))
	if err != nil {
		return false, leftoverForeign, failure("git.worktree", run.BasePath, err)
	}
	if !os.SameFile(got, want) {
		return false, leftoverDetached, nil
	}
	return true, leftoverCheckout, nil
}
