package git

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Inspect verifies persisted ownership and the current branch without fetching,
// recreating a worktree, or touching the agent's working files.
func (m *Manager) Inspect(ctx context.Context, run Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
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

// ListWorktrees includes registered linked worktrees (even missing directories)
// and unmanaged directories in the worktree layout. Nothing is pruned or deleted.
func (m *Manager) ListWorktrees(ctx context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	paths := map[string]bool{}
	bases, err := filepath.Glob(filepath.Join(m.root, "repos", "*", "base"))
	if err != nil {
		return nil, err
	}
	for _, base := range bases {
		physicalBase, err := filepath.EvalSymlinks(base)
		if err != nil {
			return nil, err
		}
		out, err := command(ctx, base, "git.worktree_list", "worktree", "list", "--porcelain", "-z")
		if err != nil {
			return nil, err
		}
		for _, field := range strings.Split(out, "\x00") {
			if path, ok := strings.CutPrefix(field, "worktree "); ok && path != physicalBase {
				paths[path] = true
			}
		}
	}
	repos, err := os.ReadDir(filepath.Join(m.root, "worktrees"))
	if err != nil {
		return nil, err
	}
	for _, repo := range repos {
		if !repo.IsDir() {
			continue
		}
		dir := filepath.Join(m.root, "worktrees", repo.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				path := filepath.Join(dir, entry.Name())
				if physical, err := filepath.EvalSymlinks(path); err == nil {
					path = physical
				}
				paths[path] = true
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := make([]string, 0, len(paths))
	for path := range paths {
		result = append(result, path)
	}
	sort.Strings(result)
	return result, nil
}
