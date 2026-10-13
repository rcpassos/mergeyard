package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/rcpassos/mergeyard/internal/pathspec"
)

func exclusionSpecs(patterns []string, exclude bool) []string {
	magic := ":(glob)"
	if exclude {
		magic = ":(exclude,glob)"
	}
	specs := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		specs = append(specs, magic+pathspec.Glob(pattern))
	}
	return specs
}

func nulPaths(output string) []string {
	if output == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(output, "\x00"), "\x00")
}

func excludedUntracked(ctx context.Context, run Run) ([]string, error) {
	if len(run.Exclude) == 0 {
		return nil, nil
	}
	args := append([]string{"ls-files", "--others", "--exclude-standard", "-z", "--"}, exclusionSpecs(run.Exclude, false)...)
	output, err := command(ctx, run.Path, "git.exclude", args...)
	return nulPaths(output), err
}

func stage(ctx context.Context, run Run) ([]Diagnostic, error) {
	var diagnostics []Diagnostic
	if len(run.Exclude) > 0 {
		parents, err := pendingMergeParents(ctx, run)
		if err != nil {
			return diagnostics, err
		}
		inherited := map[string]bool{}
		for _, parent := range parents {
			output, err := command(ctx, run.Path, "git.exclude", "ls-tree", "-r", "--name-only", "-z", parent)
			if err != nil {
				return diagnostics, err
			}
			for _, name := range nulPaths(output) {
				inherited[name] = true
			}
		}
		// Only paths absent from HEAD and every pending merge parent are
		// generated additions. A merge's tracked incoming files must survive
		// exclusions even if they are new to this branch.
		specs := exclusionSpecs(run.Exclude, false)
		args := append([]string{"diff", "--cached", "--ita-visible-in-index", "--no-renames", "--diff-filter=A", "--name-only", "-z", "HEAD", "--"}, specs...)
		output, err := command(ctx, run.Path, "git.exclude", args...)
		if err != nil {
			return diagnostics, err
		}
		for _, name := range nulPaths(output) {
			if inherited[name] {
				continue
			}
			if _, err := command(ctx, run.Path, "git.stage", "restore", "--staged", "--source=HEAD", "--", ":(literal)"+name); err != nil {
				return diagnostics, err
			}
		}
		// Restore matching tracked paths from all parents when a tool has
		// removed them from the index. Never use the current index as proof
		// that a generated addition was already tracked.
		retained := map[string]bool{}
		for _, parent := range append([]string{"HEAD"}, parents...) {
			args = append([]string{"ls-files", "--cached", "--with-tree=" + parent, "-z", "--"}, specs...)
			output, err = command(ctx, run.Path, "git.exclude", args...)
			if err != nil {
				return diagnostics, err
			}
			for _, name := range nulPaths(output) {
				retained[name] = true
			}
		}
		for name := range retained {
			info, err := os.Lstat(filepath.Join(run.Path, name))
			if errors.Is(err, os.ErrNotExist) {
				continue // add --update below stages real deletions.
			}
			if err != nil {
				return diagnostics, failure("git.stage", run.Path, err)
			}
			if info.IsDir() {
				continue // A tracked file replaced by a directory is deleted.
			}
			if _, err := command(ctx, run.Path, "git.stage", "add", "--force", "--", ":(literal)"+name); err != nil {
				return diagnostics, err
			}
		}
		leftovers, err := excludedUntracked(ctx, run)
		if err != nil {
			return diagnostics, err
		}
		for _, name := range leftovers {
			diagnostics = append(diagnostics, Diagnostic{Code: "git.excluded_untracked", Path: name, Message: "Excluded untracked file left in the worktree until cleanup"})
		}
		args = append([]string{"diff", "--no-renames", "--diff-filter=MDT", "--name-only", "-z", "HEAD", "--"}, specs...)
		output, err = command(ctx, run.Path, "git.exclude", args...)
		if err != nil {
			return diagnostics, err
		}
		for _, name := range nulPaths(output) {
			diagnostics = append(diagnostics, Diagnostic{Code: "git.exclude_tracked", Path: name, Message: "Warning: tracked change matches an exclude pattern and will still be committed"})
		}
	}
	args := append([]string{"add", "--all", "--", "."}, exclusionSpecs(run.Exclude, true)...)
	if _, err := command(ctx, run.Path, "git.stage", args...); err != nil {
		return diagnostics, err
	}
	if len(run.Exclude) > 0 {
		// Restore all tracked edits/deletions omitted by the exclusion specs.
		if _, err := command(ctx, run.Path, "git.stage", "add", "--update"); err != nil {
			return diagnostics, err
		}
	}
	return diagnostics, nil
}

// worktreeDirty keeps all tracked/index changes significant. Only non-ignored
// untracked files may be filtered, using the same Git pathspecs as staging.
func worktreeDirty(ctx context.Context, run Run) (bool, error) {
	if len(run.Exclude) == 0 {
		status, err := command(ctx, run.Path, "git.status", "status", "--porcelain", "--untracked-files=all")
		return status != "", err
	}
	dirty, err := hasDiff(ctx, run.Path, "diff", "--cached", "--quiet", "--exit-code", "--")
	if err != nil || dirty {
		return dirty, err
	}
	dirty, err = hasDiff(ctx, run.Path, "diff", "--quiet", "--exit-code", "--")
	if err != nil || dirty {
		return dirty, err
	}
	args := append([]string{"ls-files", "--others", "--exclude-standard", "-z", "--", "."}, exclusionSpecs(run.Exclude, true)...)
	output, err := command(ctx, run.Path, "git.status", args...)
	return len(nulPaths(output)) > 0, err
}

// MERGE_HEAD can contain several parents for an octopus merge. Read the
// worktree-specific metadata path so linked worktrees retain their own parents.
func pendingMergeParents(ctx context.Context, run Run) ([]string, error) {
	path, err := command(ctx, run.Path, "git.exclude", "rev-parse", "--path-format=absolute", "--git-path", "MERGE_HEAD")
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, failure("git.exclude", path, err)
	}
	parents := strings.Fields(string(data))
	if len(parents) == 0 {
		return nil, failure("git.exclude", path, errors.New("pending merge has no parent"))
	}
	for _, parent := range parents {
		if !shaPattern.MatchString(parent) {
			return nil, failure("git.exclude", path, errors.New("pending merge has an invalid parent"))
		}
	}
	return parents, nil
}
