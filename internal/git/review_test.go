package git_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	managedgit "github.com/rcpassos/mergeyard/internal/git"
)

func TestReviewRestoresGitStateAndPreservesExistingWork(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	ctx := context.Background()
	write(t, run.Path, "README.md", "staged work\n")
	git(t, run.Path, "add", "README.md")
	write(t, run.Path, "README.md", "unstaged work\n")
	write(t, run.Path, "keep.txt", "existing untracked\n")
	before, err := f.manager.SnapshotReview(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	write(t, run.Path, "README.md", "reviewer edit\n")
	write(t, run.Path, "reviewer.txt", "new reviewer file\n")
	git(t, run.Path, "add", ".")
	git(t, run.Path, "commit", "-m", "reviewer commit")
	write(t, run.Path, "build.ignored", "test output\n")
	changed, err := f.manager.ReviewChanged(ctx, run, before)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	// Restoration is safe to replay after the control plane dies midway through it.
	for range 2 {
		if err := f.manager.RestoreReview(ctx, run, before); err != nil {
			t.Fatal(err)
		}
	}
	after, err := f.manager.SnapshotReview(ctx, run)
	after.Reflog = before.Reflog
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("restore differs: before=%+v after=%+v err=%v", before, after, err)
	}
	if _, err := os.Stat(filepath.Join(run.Path, "reviewer.txt")); !os.IsNotExist(err) {
		t.Fatal("reviewer file survived")
	}
	if data, err := os.ReadFile(filepath.Join(run.Path, "build.ignored")); err != nil || string(data) != "test output\n" {
		t.Fatal("ignored test output lost")
	}
}

func TestReviewIgnoredOutputIsPermittedAndBranchSwitchIsAmbiguous(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	ctx := context.Background()
	before, err := f.manager.SnapshotReview(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	write(t, run.Path, "build.ignored", "cache")
	changed, err := f.manager.ReviewChanged(ctx, run, before)
	if err != nil || changed {
		t.Fatalf("ignored output contaminated review: %v %v", changed, err)
	}
	git(t, run.Path, "checkout", "--detach")
	requireCode(t, f.manager.RestoreReview(ctx, run, before), "git.worktree_mismatch")
	if git(t, run.Path, "rev-parse", "HEAD") != before.Head {
		t.Fatal("ambiguous restoration changed HEAD")
	}
}

var _ managedgit.ReviewSnapshot

func TestReviewRestoresModesSymlinksDeletedFilesAndWhitespaceNames(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	ctx := context.Background()
	name := " spaced\nfile.txt"
	write(t, run.Path, name, "original")
	if err := os.Chmod(filepath.Join(run.Path, name), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(name, filepath.Join(run.Path, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(run.Path, "README.md")); err != nil {
		t.Fatal(err)
	}
	before, err := f.manager.SnapshotReview(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	write(t, run.Path, name, "edited")
	os.Chmod(filepath.Join(run.Path, name), 0600)
	os.Remove(filepath.Join(run.Path, "link"))
	write(t, run.Path, "link", "regular")
	write(t, run.Path, "README.md", "restored by reviewer")
	if err := f.manager.RestoreReview(ctx, run, before); err != nil {
		t.Fatal(err)
	}
	after, err := f.manager.SnapshotReview(ctx, run)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("restore changed state: %+v %+v %v", before, after, err)
	}
}

func TestAmbiguousReviewFilesystemIsPreservedBeforeRestoring(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	ctx := context.Background()
	before, err := f.manager.SnapshotReview(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	git(t, run.Path, "commit", "--allow-empty", "-m", "reviewer commit")
	head := git(t, run.Path, "rev-parse", "HEAD")
	os.Remove(filepath.Join(run.Path, "README.md"))
	os.Mkdir(filepath.Join(run.Path, "README.md"), 0700)
	write(t, filepath.Join(run.Path, "README.md"), "preserve", "ambiguous work")
	requireCode(t, f.manager.RestoreReview(ctx, run, before), "git.review_ambiguous")
	if git(t, run.Path, "rev-parse", "HEAD") != head {
		t.Fatal("ambiguous restore changed branch before refusing")
	}
	data, err := os.ReadFile(filepath.Join(run.Path, "README.md", "preserve"))
	if err != nil || string(data) != "ambiguous work" {
		t.Fatal("ambiguous file lost")
	}
}

func TestReviewDetectsDetachedCommitAfterReturningToOwnedBranch(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	ctx := context.Background()
	before, err := f.manager.SnapshotReview(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	git(t, run.Path, "checkout", "--detach")
	write(t, run.Path, "README.md", "detached edit")
	git(t, run.Path, "add", ".")
	git(t, run.Path, "commit", "-m", "detached reviewer commit")
	git(t, run.Path, "checkout", run.Branch)
	changed, err := f.manager.ReviewChanged(ctx, run, before)
	if err != nil || !changed {
		t.Fatalf("detached reviewer commit was not detected: %v %v", changed, err)
	}
}
