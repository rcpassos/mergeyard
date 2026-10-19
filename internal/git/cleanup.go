package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Cleanup removes a completed run's worktree, metadata, and local branch. The
// caller must confirm completion; stopped, manual, and needs-attention runs must
// retain their worktrees. Dirty worktrees and unpublished local commits are preserved; remote refs are kept.
// Repeating cleanup after an interrupted or completed cleanup is safe.
func (m *Manager) Cleanup(ctx context.Context, run Run) error {
	// Worktree removal and publication checks keep their own class deadlines.
	ctx, release, err := m.begin(ctx, m.timeouts.Metadata+m.timeouts.Worktree+m.timeouts.Fetch)
	if err != nil {
		return err
	}
	defer release()
	if err := m.validateRun(run); err != nil {
		return err
	}
	worktreeCtx, worktreeCancel := m.worktreeDeadline(ctx)
	err = m.cleanupWorktree(worktreeCtx, run)
	worktreeCancel()
	if err != nil {
		return err
	}
	fetchCtx, fetchCancel := m.fetchDeadline(ctx)
	defer fetchCancel()
	return m.cleanupBranch(fetchCtx, run)
}

// CleanupWorktree removes only a clean owned tree and prunes its registration.
// A missing directory is accepted only by callers with recorded removal intent.
func (m *Manager) CleanupWorktree(ctx context.Context, run Run) error {
	ctx, release, err := m.begin(ctx, m.timeouts.Worktree)
	if err != nil {
		return err
	}
	defer release()
	return m.cleanupWorktree(ctx, run)
}
func (m *Manager) cleanupWorktree(ctx context.Context, run Run) error {
	if err := m.validateRun(run); err != nil {
		return err
	}
	if err := verifyBase(ctx, run.BasePath); err != nil {
		return err
	}
	if _, err := verifyOrigin(ctx, run); err != nil {
		return err
	}
	if err := cleanupPublished(ctx, run); err != nil {
		return err
	}
	removalStarted, err := cleanupStarted(run)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(run.Path); err == nil {
		linkGone := false
		if _, err := os.Lstat(filepath.Join(run.Path, ".git")); errors.Is(err, os.ErrNotExist) {
			linkGone = true
		} else if err != nil {
			return failure("git.cleanup", run.Path, err)
		}
		if removalStarted || (linkGone && run.RemovalIntent) {
			// A recorded removal already proved ownership, publication, and
			// cleanliness. Finish it only when the worktree was actually
			// changed by that removal, so a refusal that deleted nothing
			// cannot discard later human work.
			if err := m.finishRemoval(ctx, run); err != nil {
				return err
			}
		} else if err := m.removeLinkedWorktree(ctx, run); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return failure("git.cleanup", run.Path, err)
	}
	if _, err := command(ctx, run.BasePath, "git.cleanup", "worktree", "prune", "--expire", "now"); err != nil {
		return err
	}
	if removalStarted || run.RemovalIntent {
		registered, _, err := worktreeState(ctx, run)
		if err != nil {
			return err
		}
		if registered {
			// Prune keeps a locked registration. Report it instead of
			// recording removal and leaving branch deletion to fail forever.
			return failure("git.cleanup", run.Path, errors.New("worktree registration remains after removal"))
		}
	}
	return removeCleanupIntent(run)
}

// removeLinkedWorktree removes a verified, clean checkout. It records the
// cleanup boundary only after cleanliness is proven and before Git starts
// deleting; a refusal that deletes nothing clears it again below.
func (m *Manager) removeLinkedWorktree(ctx context.Context, run Run) error {
	if err := verifyWorktree(ctx, run); err != nil {
		return err
	}
	dirty, err := worktreeDirty(ctx, run)
	if err != nil {
		return err
	}
	if dirty {
		return failure("git.cleanup", run.Path, errors.New("dirty work is preserved for human attention"))
	}
	leftovers, err := excludedUntracked(ctx, run)
	if err != nil {
		return err
	}
	args := []string{"worktree", "remove"}
	if len(leftovers) > 0 {
		// Only configured untracked leftovers remain. Ownership and
		// publication checks above still guard the complete worktree.
		args = append(args, "--force")
	}
	args = append(args, "--", run.Path)
	if err := writeCleanupIntent(run); err != nil {
		return err
	}
	if _, err := command(ctx, run.BasePath, "git.cleanup", args...); err != nil {
		// Git may have refused before deleting anything, such as for a locked
		// worktree. Keep the boundary only when the removal left evidence;
		// otherwise a later retry would treat human edits as leftovers.
		if progress, checkErr := removalProgress(ctx, run); checkErr == nil && !progress {
			_ = removeCleanupIntent(run)
		}
		return err
	}
	return nil
}

// finishRemoval completes a removal with a recorded boundary. An intact tree
// is rechecked first: a lock is human protection, and changes other than the
// tracked deletions a killed removal leaves mean the tree is no longer the
// clean checkout that removal authorized.
func (m *Manager) finishRemoval(ctx context.Context, run Run) error {
	_, locked, err := worktreeState(ctx, run)
	if err != nil {
		return err
	}
	if locked {
		return failure("git.cleanup", run.Path, errors.New("worktree is locked; removal preserved for human attention"))
	}
	if _, err := os.Lstat(filepath.Join(run.Path, ".git")); errors.Is(err, os.ErrNotExist) {
		return removeTree(ctx, run.Path)
	} else if err != nil {
		return failure("git.cleanup", run.Path, err)
	}
	if err := verifyWorktree(ctx, run); err != nil {
		return err
	}
	progress, err := removalProgress(ctx, run)
	if err != nil {
		return err
	}
	if progress {
		return removeTree(ctx, run.Path)
	}
	dirty, err := worktreeDirty(ctx, run)
	if err != nil {
		return err
	}
	if dirty {
		return failure("git.cleanup", run.Path, errors.New("worktree changed after removal started; preserved for human attention"))
	}
	// The removal deleted nothing, so retry the ordinary path while the link
	// is still readable. Git keeps its own lock and cleanliness safeguards.
	return m.removeLinkedWorktree(ctx, run)
}

// removalProgress reports whether the worktree differs from HEAD only by
// tracked deletions and untracked files the run already excludes. A killed
// removal leaves that shape; any edit or added file means human work arrived
// after removal was authorized.
func removalProgress(ctx context.Context, run Run) (bool, error) {
	status, err := command(ctx, run.Path, "git.status", "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return false, err
	}
	excluded := map[string]bool{}
	if len(run.Exclude) > 0 {
		leftovers, err := excludedUntracked(ctx, run)
		if err != nil {
			return false, err
		}
		for _, path := range leftovers {
			excluded[path] = true
		}
	}
	progress := false
	for _, record := range strings.Split(status, "\x00") {
		if record == "" {
			continue
		}
		if len(record) < 3 {
			return false, nil
		}
		code, path := record[:2], record[3:]
		switch {
		case code == " D":
			progress = true
		case code == "??" && excluded[path]:
			// An allowed leftover does not prove deletion started, but it
			// does not block finishing one that did.
		default:
			return false, nil
		}
	}
	return progress, nil
}

// cleanupIntent records where a deletion started after ownership, publication,
// and cleanliness checks passed. Its presence makes the leftover files part of
// an authorized removal regardless of whether Git's link survived.
type cleanupIntent struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	BasePath string `json:"base_path"`
	Branch   string `json:"branch"`
}

func cleanupIntentPath(run Run) string {
	return filepath.Join(filepath.Dir(run.Path), run.ID+".cleanup")
}

func newCleanupIntent(run Run) cleanupIntent {
	return cleanupIntent{ID: run.ID, Path: run.Path, BasePath: run.BasePath, Branch: run.Branch}
}

func cleanupStarted(run Run) (bool, error) {
	data, err := os.ReadFile(cleanupIntentPath(run))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, failure("git.cleanup", run.Path, err)
	}
	var intent cleanupIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		return false, failure("git.cleanup", run.Path, fmt.Errorf("invalid cleanup intent: %w", err))
	}
	if intent != newCleanupIntent(run) {
		return false, failure("git.cleanup", run.Path, errors.New("cleanup intent does not match the run"))
	}
	return true, nil
}

func writeCleanupIntent(run Run) error {
	if err := writeAtomicJSON(cleanupIntentPath(run), newCleanupIntent(run)); err != nil {
		return failure("git.cleanup", run.Path, err)
	}
	return nil
}

func removeCleanupIntent(run Run) error {
	if err := os.Remove(cleanupIntentPath(run)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return failure("git.cleanup", run.Path, err)
	}
	return nil
}

// removeTree deletes path recursively, checking the managed context between
// entries so an expired deadline or cancellation stops the work before the
// manager releases its gate.
func removeTree(ctx context.Context, path string) error {
	return removeTreeEntries(ctx, path, os.ReadDir)
}

func removeTreeEntries(ctx context.Context, path string, readDir func(string) ([]os.DirEntry, error)) error {
	if err := ctx.Err(); err != nil {
		return contextFailure(ctx, "git.cleanup", path, err)
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return failure("git.cleanup", path, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return failure("git.cleanup", path, err)
		}
		return nil
	}
	entries, err := readDir(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return failure("git.cleanup", path, err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return contextFailure(ctx, "git.cleanup", path, err)
		}
		if err := removeTreeEntries(ctx, filepath.Join(path, entry.Name()), readDir); err != nil {
			return err
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return failure("git.cleanup", path, err)
	}
	return nil
}

// worktreeRegistered reports whether the base still lists a worktree at the
// run's path after pruning. Git prints physical paths, so resolve the parent
// directory to compare against the managed path.
// worktreeState reports whether the base still lists a worktree at the run's
// path after pruning, and whether that registration is locked. Git prints
// physical paths, so resolve the parent directory to compare against the
// managed path.
func worktreeState(ctx context.Context, run Run) (registered, locked bool, err error) {
	out, err := command(ctx, run.BasePath, "git.cleanup", "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return false, false, err
	}
	path := run.Path
	if parent, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		path = filepath.Join(parent, filepath.Base(path))
	}
	ours := false
	for _, field := range strings.Split(out, "\x00") {
		if worktree, ok := strings.CutPrefix(field, "worktree "); ok {
			ours = worktree == path
			registered = registered || ours
			continue
		}
		if ours && (field == "locked" || strings.HasPrefix(field, "locked ")) {
			locked = true
		}
	}
	return registered, locked, nil
}

// CleanupBranch deletes a completed run's published local ref; remote refs are retained.
func (m *Manager) CleanupBranch(ctx context.Context, run Run) error {
	ctx, release, err := m.begin(ctx, m.timeouts.Fetch)
	if err != nil {
		return err
	}
	defer release()
	return m.cleanupBranch(ctx, run)
}
func (m *Manager) cleanupBranch(ctx context.Context, run Run) error {
	if err := m.validateRun(run); err != nil {
		return err
	}
	if err := verifyBase(ctx, run.BasePath); err != nil {
		return err
	}
	if _, err := verifyOrigin(ctx, run); err != nil {
		return err
	}
	exists, err := refExists(ctx, run.BasePath, "refs/heads/"+run.Branch)
	if err != nil || !exists {
		return err
	}
	if err := cleanupPublished(ctx, run); err != nil {
		return err
	}
	// A completed PR may have been squash-merged, so ancestry against the local
	// base is not a valid completion check. This deletes only the local branch.
	_, err = command(ctx, run.BasePath, "git.cleanup", "branch", "-D", "--", run.Branch)
	return err
}

// Clean files do not imply published commits. Check the ref before removing its
// tree and again before deleting it, including after interrupted cleanup.
func cleanupPublished(ctx context.Context, run Run) error {
	exists, err := refExists(ctx, run.BasePath, "refs/heads/"+run.Branch)
	if err != nil || !exists {
		return err
	}
	head, err := command(ctx, run.BasePath, "git.ref", "rev-parse", "refs/heads/"+run.Branch)
	if err != nil {
		return err
	}
	// Preparation pinned this commit from the fetched public base.
	if head == run.BaseSHA {
		return nil
	}
	if shaPattern.MatchString(run.PublishedSHA) {
		contained, err := publishedContains(ctx, run, head, run.PublishedSHA)
		if err == nil && contained {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	origin, err := verifyOrigin(ctx, run)
	if err != nil {
		return err
	}
	out, err := command(ctx, run.BasePath, "git.cleanup_publication", "ls-remote", "--heads", "--", origin, "refs/heads/"+run.Branch)
	if err != nil {
		return err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return failure("git.unpublished_work", run.Path, errors.New("local commits have no confirmed published head; worktree and branch are preserved for human attention"))
	}
	if len(fields) != 2 || !shaPattern.MatchString(fields[0]) || fields[1] != "refs/heads/"+run.Branch {
		return failure("git.cleanup_publication", run.Path, errors.New("remote branch publication is ambiguous"))
	}
	published := fields[0]
	if head == published {
		return nil
	}
	if _, err := command(ctx, run.BasePath, "git.cleanup_publication", "cat-file", "-e", published+"^{commit}"); err != nil {
		if _, err := command(ctx, run.BasePath, "git.cleanup_publication", "fetch", "--no-tags", "--", origin, published); err != nil {
			return err
		}
	}
	contained, err := publishedContains(ctx, run, head, published)
	if err != nil {
		return err
	}
	if !contained {
		return failure("git.unpublished_work", run.Path, errors.New("local commits are not contained in the published head; worktree and branch are preserved for human attention"))
	}
	return nil
}
func publishedContains(ctx context.Context, run Run, head, published string) (bool, error) {
	_, err := command(ctx, run.BasePath, "git.cleanup_publication", "merge-base", "--is-ancestor", head, published)
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if ctx.Err() == nil && errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}
