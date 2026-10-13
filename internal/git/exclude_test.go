package git_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	managedgit "github.com/rcpassos/mergeyard/internal/git"
)

func TestPhaseExcludesUntrackedPatternsButCommitsTrackedChanges(t *testing.T) {
	for _, tc := range []struct{ name, pattern, excluded, included string }{
		{"directory", ".venv/", ".venv/bin/python", ".venv.txt"},
		{"glob", "*.har", "capture.har", "nested/capture.har"},
		{"nested", "**/traces/**", "nested/traces/deep/trace.zip", "nested/trace.zip"},
		{"wildcard directory", "**/cache/", "nested/cache/deep/file", "nested/cache.txt"},
		{"character class", "capture[[:digit:]].har", "capture1.har", "capturex.har"},
		{"unusual filename", "*.har", " space\nline.har", "keep.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			// Seed tracked paths that also match exclusions, including a deletion.
			for _, name := range []string{tc.excluded, "deleted.har"} {
				writeNested(t, f.seed, name, "tracked")
			}
			git(t, f.seed, "add", "--all")
			git(t, f.seed, "commit", "-m", "tracked artifacts")
			git(t, f.seed, "push", "origin", "main")
			run := prepare(t, f)
			run.Exclude = []string{tc.pattern, "deleted.har", "new.har"}
			configBefore, err := os.ReadFile(filepath.Join(run.BasePath, ".git/config"))
			if err != nil {
				t.Fatal(err)
			}
			infoBefore, err := os.ReadFile(filepath.Join(run.BasePath, ".git/info/exclude"))
			if err != nil {
				t.Fatal(err)
			}
			writeNested(t, run.Path, tc.excluded, "tracked edit")
			if err := os.Remove(filepath.Join(run.Path, "deleted.har")); err != nil {
				t.Fatal(err)
			}
			writeNested(t, run.Path, tc.included, "included new file")
			writeNested(t, run.Path, "new.har", "excluded new file")
			git(t, run.Path, "add", "new.har") // A tool may have staged a new artifact already.
			writeNested(t, run.Path, "other.har", "untracked artifact")
			run.Exclude = append(run.Exclude, "other.har")
			result, err := f.manager.CommitAndPush(context.Background(), run, managedgit.Phase{Title: "implement", RequireChanges: true})
			if err != nil {
				t.Fatal(err)
			}
			if git(t, run.Path, "show", "HEAD:"+tc.excluded) != "tracked edit" || git(t, run.Path, "show", "HEAD:"+tc.included) != "included new file" {
				t.Fatal("tracked edit or allowed new file missing")
			}
			if git(t, run.Path, "ls-tree", "--name-only", "HEAD", "deleted.har", "new.har", "other.har") != "" {
				t.Fatal("excluded addition or tracked deletion committed incorrectly")
			}
			got := map[string]string{}
			for _, d := range result.Diagnostics {
				got[d.Path] = d.Code
			}
			want := map[string]string{tc.excluded: "git.exclude_tracked", "deleted.har": "git.exclude_tracked", "new.har": "git.excluded_untracked", "other.har": "git.excluded_untracked"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("diagnostics: %#v; want %#v", got, want)
			}
			for path, before := range map[string][]byte{".git/config": configBefore, ".git/info/exclude": infoBefore} {
				after, err := os.ReadFile(filepath.Join(run.BasePath, path))
				if err != nil || string(after) != string(before) {
					t.Fatalf("Git settings modified: %s %v", path, err)
				}
			}
			if git(t, run.Path, "show", "HEAD:.gitignore") != "*.ignored" {
				t.Fatal("repository ignore file modified")
			}
			if err := f.manager.InspectFixTarget(context.Background(), run, result.SHA); err != nil {
				t.Fatalf("leftovers blocked pinned checks: %v", err)
			}
			if _, err := f.manager.InspectRetry(context.Background(), run, result.SHA, false); err != nil {
				t.Fatalf("leftovers blocked retry: %v", err)
			}
			if err := f.manager.Cleanup(context.Background(), run); err != nil {
				t.Fatalf("leftovers blocked cleanup: %v", err)
			}
			if _, err := os.Stat(run.Path); !os.IsNotExist(err) {
				t.Fatalf("worktree leftovers retained: %v", err)
			}
		})
	}
}

func TestExcludedOnlyImplementationHasNoChanges(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	run.Exclude = []string{".venv/"}
	writeNested(t, run.Path, ".venv/bin/python", "tool output")
	result, err := f.manager.CommitImplement(context.Background(), run, managedgit.Phase{Title: "implement", RequireChanges: true})
	requireCode(t, err, "git.no_changes")
	if result.Committed || len(result.Diagnostics) != 1 || result.Diagnostics[0].Path != ".venv/bin/python" {
		t.Fatalf("excluded-only result: %+v", result)
	}
	if git(t, run.Path, "rev-parse", "HEAD") != run.BaseSHA {
		t.Fatal("created commit for leftovers")
	}
}

func TestExcludedIntentToAddIsNotCommitted(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	run.Exclude = []string{"*.har"}
	write(t, run.Path, "capture.har", "artifact")
	git(t, run.Path, "add", "--intent-to-add", "capture.har")
	result, err := f.manager.CommitImplement(context.Background(), run, managedgit.Phase{Title: "implement", RequireChanges: true})
	requireCode(t, err, "git.no_changes")
	if result.Committed || len(result.Diagnostics) != 1 || git(t, run.Path, "ls-files", "capture.har") != "" {
		t.Fatalf("intent-to-add exclusion: %+v", result)
	}
}

func TestExcludedTrackedFileRemovedFromIndexStillCommitsItsEdit(t *testing.T) {
	for _, allPaths := range []bool{false, true} {
		t.Run(map[bool]string{false: "one path", true: "all paths"}[allPaths], func(t *testing.T) {
			f := newFixture(t)
			write(t, f.seed, "capture.har", "original")
			git(t, f.seed, "add", "capture.har")
			git(t, f.seed, "commit", "-m", "tracked capture")
			git(t, f.seed, "push", "origin", "main")
			run := prepare(t, f)
			run.Exclude = []string{"*.har"}
			if allPaths {
				run.Exclude = []string{"**"}
				git(t, run.Path, "rm", "-r", "--cached", ".")
			} else {
				git(t, run.Path, "rm", "--cached", "capture.har")
			}
			write(t, run.Path, "capture.har", "tracked edit")
			result, err := f.manager.CommitImplement(context.Background(), run, managedgit.Phase{Title: "implement", RequireChanges: true})
			if err != nil {
				t.Fatal(err)
			}
			if got := git(t, run.Path, "show", "HEAD:capture.har"); got != "tracked edit" {
				t.Fatalf("tracked change lost: %q", got)
			}
			if len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != "git.exclude_tracked" {
				t.Fatalf("tracked warning missing or misclassified: %+v", result.Diagnostics)
			}
		})
	}
}
func TestExclusionsNeverHideTrackedOrNonExcludedDirtyWork(t *testing.T) {
	for _, name := range []string{"README.md", "keep.txt"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			run := prepare(t, f)
			run.Exclude = []string{"README.md", "*.har"}
			write(t, run.Path, name, "preserve")
			write(t, run.Path, "capture.har", "leftover")
			requireCode(t, f.manager.InspectFixTarget(context.Background(), run, run.BaseSHA), "review.head_changed")
			requireCode(t, f.manager.Cleanup(context.Background(), run), "git.cleanup")
			if data, err := os.ReadFile(filepath.Join(run.Path, name)); err != nil || string(data) != "preserve" {
				t.Fatal("discarded dirty work")
			}
		})
	}
}

func TestExclusionsDoNotHideIndexChangesCanceledInWorkingTree(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	run.Exclude = []string{"README.md"}
	write(t, run.Path, "README.md", "staged change")
	git(t, run.Path, "add", "README.md")
	write(t, run.Path, "README.md", "initial\n")
	requireCode(t, f.manager.InspectFixTarget(context.Background(), run, run.BaseSHA), "review.head_changed")
	requireCode(t, f.manager.Cleanup(context.Background(), run), "git.cleanup")
}

func TestFixExcludesUntrackedLeftovers(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	run.Exclude = []string{"*.har"}
	write(t, run.Path, "README.md", "implementation")
	first, err := f.manager.CommitAndPush(context.Background(), run, managedgit.Phase{Title: "implement", RequireChanges: true})
	if err != nil {
		t.Fatal(err)
	}
	write(t, run.Path, "README.md", "fixed")
	write(t, run.Path, "capture.har", "fix leftover")
	fixed, err := f.manager.CommitFix(context.Background(), run, managedgit.Phase{Title: "fix"}, first.SHA)
	if err != nil {
		t.Fatal(err)
	}
	if !fixed.TreeChanged || len(fixed.Diagnostics) != 1 {
		t.Fatalf("fix: %+v", fixed)
	}
	if err := f.manager.PushFix(context.Background(), run, first.SHA, fixed.SHA); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(git(t, f.remote, "ls-tree", "--name-only", run.Branch), "capture.har") {
		t.Fatal("pushed excluded fix file")
	}
}

func writeNested(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, dir, name, content)
}

func TestConflictRepairExclusionsPreserveTrackedMergeParentFiles(t *testing.T) {
	for _, mode := range []string{"merge index", "removed from index", "intent to add generated"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			run := prepare(t, f)
			run.Exclude = []string{"*.har"}
			write(t, run.Path, "README.md", "approved\n")
			approved, err := f.manager.CommitAndPush(context.Background(), run, managedgit.Phase{Title: "implement", RequireChanges: true})
			if err != nil {
				t.Fatal(err)
			}
			write(t, f.seed, "README.md", "base change\n")
			write(t, f.seed, "capture.har", "tracked base capture\n")
			git(t, f.seed, "add", ".")
			git(t, f.seed, "commit", "-m", "advance base with tracked capture")
			git(t, f.seed, "push", "origin", "main")
			base, err := f.manager.CheckBase(context.Background(), run, "main", approved.SHA)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.manager.PrepareBaseRepair(context.Background(), run, approved.SHA, base.BaseSHA); err != nil {
				t.Fatal(err)
			}
			write(t, run.Path, "README.md", "approved\n")
			if mode == "removed from index" {
				git(t, run.Path, "rm", "--cached", "capture.har")
			}
			write(t, run.Path, "generated.har", "new generated output\n")
			if mode == "intent to add generated" {
				git(t, run.Path, "add", "--intent-to-add", "generated.har")
			} else {
				git(t, run.Path, "add", "generated.har")
			}
			fixed, err := f.manager.CommitFix(context.Background(), run, managedgit.Phase{Title: "fix conflict"}, approved.SHA)
			if err != nil {
				t.Fatal(err)
			}
			if got := git(t, run.Path, "ls-tree", "--name-only", fixed.SHA, "capture.har"); got != "capture.har" {
				t.Fatalf("tracked merge-parent file dropped: %q", got)
			}
			if got := git(t, run.Path, "show", fixed.SHA+":capture.har"); got != "tracked base capture" {
				t.Fatalf("base contents lost: %q", got)
			}
			if got := git(t, run.Path, "ls-tree", "--name-only", fixed.SHA, "generated.har"); got != "" {
				t.Fatal("generated excluded file committed")
			}
			for _, d := range fixed.Diagnostics {
				if d.Path == "capture.har" && d.Code == "git.excluded_untracked" {
					t.Fatal("tracked base file misclassified as generated")
				}
			}
			git(t, run.Path, "merge-base", "--is-ancestor", base.BaseSHA, fixed.SHA)
			if err := f.manager.PushFix(context.Background(), run, approved.SHA, fixed.SHA); err != nil {
				t.Fatal(err)
			}
			if got := git(t, f.remote, "show", run.Branch+":capture.har"); got != "tracked base capture" {
				t.Fatal("published merge dropped base addition")
			}
		})
	}
}

func TestConflictRepairExclusionsAllowFileInsteadOfIncomingDirectory(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	run.Exclude = []string{"**/*.har"}
	write(t, run.Path, "cache", "approved cache\n")
	approved, err := f.manager.CommitAndPush(context.Background(), run, managedgit.Phase{Title: "implement", RequireChanges: true})
	if err != nil {
		t.Fatal(err)
	}
	writeNested(t, f.seed, "cache/data.har", "tracked base capture\n")
	git(t, f.seed, "add", ".")
	git(t, f.seed, "commit", "-m", "advance base with cache directory")
	git(t, f.seed, "push", "origin", "main")
	base, err := f.manager.CheckBase(context.Background(), run, "main", approved.SHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.manager.PrepareBaseRepair(context.Background(), run, approved.SHA, base.BaseSHA); err != nil {
		t.Fatal(err)
	}
	// Git relocates the approved file to cache~HEAD during the directory/file
	// conflict. Resolve it by retaining the approved file at its original path.
	for _, name := range []string{"cache", "cache~HEAD"} {
		if err := os.RemoveAll(filepath.Join(run.Path, name)); err != nil {
			t.Fatal(err)
		}
	}
	write(t, run.Path, "cache", "approved cache\n")
	git(t, run.Path, "add", "--all")
	write(t, run.Path, "generated.har", "generated output\n")
	git(t, run.Path, "add", "generated.har")
	fixed, err := f.manager.CommitFix(context.Background(), run, managedgit.Phase{Title: "fix conflict"}, approved.SHA)
	if err != nil {
		t.Fatal(err)
	}
	if got := git(t, run.Path, "show", fixed.SHA+":cache"); got != "approved cache" {
		t.Fatalf("approved file lost: %q", got)
	}
	if got := git(t, run.Path, "ls-tree", "-r", "--name-only", fixed.SHA, "cache/data.har", "cache~HEAD", "generated.har"); got != "" {
		t.Fatalf("deleted or generated paths committed: %q", got)
	}
	git(t, run.Path, "merge-base", "--is-ancestor", base.BaseSHA, fixed.SHA)
	if err := f.manager.PushFix(context.Background(), run, approved.SHA, fixed.SHA); err != nil {
		t.Fatal(err)
	}
	if got := git(t, f.remote, "show", run.Branch+":cache"); got != "approved cache" {
		t.Fatalf("published merge lost approved file: %q", got)
	}
}
