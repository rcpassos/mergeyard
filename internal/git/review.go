package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// ReviewSnapshot retains HEAD, the index tree, and exact tracked/non-ignored
// working files outside the repository. Missing tracked files stay missing.
// Reflog detects commits even if the reviewer subsequently resets HEAD.
type ReviewSnapshot struct {
	RunID  string                `json:"run_id"`
	Head   string                `json:"head"`
	Index  string                `json:"index"`
	Reflog string                `json:"reflog"`
	Files  map[string]ReviewFile `json:"files"`
}
type ReviewFile struct {
	Data []byte      `json:"data"`
	Mode fs.FileMode `json:"mode"`
	Link string      `json:"link,omitempty"`
}

func (m *Manager) SnapshotReview(ctx context.Context, run Run) (ReviewSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotReview(ctx, run)
}
func (m *Manager) snapshotReview(ctx context.Context, run Run) (ReviewSnapshot, error) {
	snapshot := ReviewSnapshot{RunID: run.ID, Files: map[string]ReviewFile{}}
	if err := m.inspectReview(ctx, run); err != nil {
		return snapshot, err
	}
	var err error
	snapshot.Head, err = command(ctx, run.Path, "git.review_snapshot", "rev-parse", "HEAD")
	if err != nil {
		return snapshot, err
	}
	snapshot.Index, err = command(ctx, run.Path, "git.review_snapshot", "write-tree")
	if err != nil {
		return snapshot, err
	}
	log, err := command(ctx, run.Path, "git.review_snapshot", "reflog", "show", "--format=%H %gD %gs", "refs/heads/"+run.Branch)
	if err != nil {
		return snapshot, err
	}
	headLog, err := command(ctx, run.Path, "git.review_snapshot", "reflog", "show", "--format=%H %gD %gs", "HEAD")
	if err != nil {
		return snapshot, err
	}
	hash := sha256.Sum256([]byte(log + "\nHEAD\n" + headLog))
	snapshot.Reflog = hex.EncodeToString(hash[:])
	names, err := reviewFiles(ctx, run.Path)
	if err != nil {
		return snapshot, err
	}
	for _, name := range names {
		path, err := reviewPath(run.Path, name)
		if err != nil {
			return snapshot, err
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return snapshot, failure("git.review_snapshot", path, err)
		}
		file := ReviewFile{Mode: info.Mode()}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			file.Link, err = os.Readlink(path)
		case info.Mode().IsRegular():
			file.Data, err = os.ReadFile(path)
		default:
			return snapshot, failure("git.review_ambiguous", path, errors.New("submodules and special files require manual review"))
		}
		if err != nil {
			return snapshot, failure("git.review_snapshot", path, err)
		}
		snapshot.Files[name] = file
	}
	return snapshot, nil
}
func (m *Manager) inspectReview(ctx context.Context, run Run) error {
	if err := m.validateRun(run); err != nil {
		return err
	}
	if err := verifyBase(ctx, run.BasePath); err != nil {
		return err
	}
	if _, err := verifyOrigin(ctx, run); err != nil {
		return err
	}
	return verifyWorktree(ctx, run)
}
func (m *Manager) ReviewDiff(ctx context.Context, run Run, head string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.inspectReview(ctx, run); err != nil {
		return "", err
	}
	if !shaPattern.MatchString(head) {
		return "", failure("git.review_ambiguous", run.Path, errors.New("invalid review target"))
	}
	return command(ctx, run.Path, "git.review_diff", "diff", "--no-ext-diff", "--no-textconv", run.BaseSHA+"..."+head, "--")
}
func (m *Manager) ReviewChanged(ctx context.Context, run Run, before ReviewSnapshot) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if before.RunID != run.ID {
		return false, failure("git.review_ambiguous", run.Path, errors.New("snapshot belongs to another run"))
	}
	after, err := m.snapshotReview(ctx, run)
	return !reflect.DeepEqual(before, after), err
}

// RestoreReview is idempotent. It never cleans ignored output or other refs,
// checks ownership before touching files, and refuses unsafe filesystem shapes.
// The scheduler durably records contamination BEFORE calling this method.
func (m *Manager) RestoreReview(ctx context.Context, run Run, before ReviewSnapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.inspectReview(ctx, run); err != nil {
		return err
	}
	if before.RunID != run.ID || !shaPattern.MatchString(before.Head) || !shaPattern.MatchString(before.Index) {
		return failure("git.review_ambiguous", run.Path, errors.New("invalid snapshot ownership"))
	}
	names, err := reviewFiles(ctx, run.Path)
	if err != nil {
		return err
	}
	// Preflight every path before making changes. Never follow a symlink parent.
	for name := range before.Files {
		names = append(names, name)
	}
	for _, name := range names {
		path, err := reviewPath(run.Path, name)
		if err != nil {
			return err
		}
		if info, err := os.Lstat(path); err == nil && info.IsDir() {
			return failure("git.review_ambiguous", path, errors.New("file replaced with directory"))
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return failure("git.review_ambiguous", path, err)
		}
	}
	head, err := command(ctx, run.Path, "git.review_restore", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head != before.Head {
		if _, err := command(ctx, run.Path, "git.review_restore", "update-ref", "-m", "mergeyard: restore review target", "refs/heads/"+run.Branch, before.Head, head); err != nil {
			return err
		}
	}
	for name, file := range before.Files {
		path, _ := reviewPath(run.Path, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return failure("git.review_restore", path, err)
		}
		if info, err := os.Lstat(path); err == nil {
			if info.IsDir() {
				return failure("git.review_ambiguous", path, errors.New("file replaced with directory"))
			}
			if err := os.Remove(path); err != nil {
				return failure("git.review_restore", path, err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return failure("git.review_restore", path, err)
		}
		if file.Mode&os.ModeSymlink != 0 {
			err = os.Symlink(file.Link, path)
		} else {
			err = os.WriteFile(path, file.Data, file.Mode.Perm())
			if err == nil {
				err = os.Chmod(path, file.Mode.Perm())
			}
		}
		if err != nil {
			return failure("git.review_restore", path, err)
		}
	}
	// Restore ignore rules and index before enumerating newly created files.
	if _, err := command(ctx, run.Path, "git.review_restore", "read-tree", before.Index); err != nil {
		return err
	}
	current, err := reviewFiles(ctx, run.Path)
	if err != nil {
		return err
	}
	current = append(current, names...)
	for _, name := range current {
		if _, keep := before.Files[name]; keep {
			continue
		}
		path, err := reviewPath(run.Path, name)
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return failure("git.review_restore", path, err)
		}
	}
	after, err := m.snapshotReview(ctx, run)
	if err != nil {
		return err
	}
	// Restoring the branch adds a reflog entry; preserve that audit history.
	after.Reflog = before.Reflog
	if !reflect.DeepEqual(before, after) {
		return failure("git.review_restore", run.Path, errors.New("restored Git state differs from snapshot"))
	}
	return nil
}
func reviewFiles(ctx context.Context, root string) ([]string, error) {
	out, err := command(ctx, root, "git.review_snapshot", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	names := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	if out == "" {
		names = nil
	}
	sort.Strings(names)
	return names, nil
}
func reviewPath(root, name string) (string, error) {
	if name == "" || !filepath.IsLocal(name) || filepath.Clean(name) != name || name == ".git" || strings.HasPrefix(name, ".git/") {
		return "", failure("git.review_ambiguous", root, errors.New("unsafe snapshot path"))
	}
	path := filepath.Join(root, name)
	for parent := filepath.Dir(path); parent != root; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", failure("git.review_ambiguous", parent, err)
		}
		if !info.IsDir() {
			return "", failure("git.review_ambiguous", parent, errors.New("snapshot path parent is not a directory"))
		}
	}
	return path, nil
}
