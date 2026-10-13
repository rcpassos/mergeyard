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
		// Exclusions apply to paths absent from HEAD, even if a tool has
		// already added them to the index. Disable rename detection so an
		// excluded rename destination is treated as a new path too.
		specs := exclusionSpecs(run.Exclude, false)
		args := append([]string{"diff", "--cached", "--ita-visible-in-index", "--no-renames", "--diff-filter=A", "--name-only", "-z", "HEAD", "--"}, specs...)
		output, err := command(ctx, run.Path, "git.exclude", args...)
		if err != nil {
			return diagnostics, err
		}
		for _, name := range nulPaths(output) {
			if _, err := command(ctx, run.Path, "git.stage", "restore", "--staged", "--source=HEAD", "--", ":(literal)"+name); err != nil {
				return diagnostics, err
			}
		}
		// HEAD, rather than the current index, decides which files were
		// already tracked. A tool may have removed a retained file from the
		// index. After excluded additions are unstaged, --with-tree adds
		// back only matching paths missing from the index but present in HEAD.
		args = append([]string{"ls-files", "--cached", "--with-tree=HEAD", "-z", "--"}, specs...)
		output, err = command(ctx, run.Path, "git.exclude", args...)
		if err != nil {
			return diagnostics, err
		}
		for _, name := range nulPaths(output) {
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
