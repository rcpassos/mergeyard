package git_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBaseMergeUsesFetchedBaseAndIsolatesVerification(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	write(t, run.Path, "feature.txt", "feature\n")
	git(t, run.Path, "add", ".")
	git(t, run.Path, "commit", "-m", "feature")
	head := git(t, run.Path, "rev-parse", "HEAD")
	write(t, f.seed, "base.txt", "current base\n")
	git(t, f.seed, "add", ".")
	git(t, f.seed, "commit", "-m", "base advance")
	git(t, f.seed, "push", "origin", "main")
	base := git(t, f.seed, "rev-parse", "HEAD")
	write(t, run.Path, "manual.txt", "preserve\n")
	merge, err := f.manager.CheckBase(context.Background(), run, "main", head)
	if err != nil {
		t.Fatal(err)
	}
	if merge.BaseSHA != base || len(merge.Conflicts) != 0 {
		t.Fatalf("merge=%+v", merge)
	}
	commit, err := f.manager.VerificationCommit(context.Background(), run, merge)
	if err != nil {
		t.Fatal(err)
	}
	scratch, err := f.manager.EnsureVerification(context.Background(), run, "check-1", commit)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"feature.txt", "base.txt"} {
		if _, err := os.Stat(filepath.Join(scratch.Path, path)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.manager.InspectVerification(context.Background(), scratch, commit); err != nil {
		t.Fatal(err)
	}
	write(t, scratch.Path, "generated.txt", "disposable\n")
	for range 2 {
		if err := f.manager.RemoveVerification(context.Background(), run, "check-1", commit); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(scratch.Path); !os.IsNotExist(err) {
		t.Fatalf("scratch not removed: %v", err)
	}
	if got := git(t, run.Path, "rev-parse", "HEAD"); got != head {
		t.Fatal("run head changed")
	}
	if data, err := os.ReadFile(filepath.Join(run.Path, "manual.txt")); err != nil || string(data) != "preserve\n" {
		t.Fatal("run edits changed")
	}
	if got := git(t, f.remote, "rev-parse", "main"); got != base {
		t.Fatal("verification pushed changes")
	}
}

func TestBaseMergeReportsConflictingPathsIncludingNewlines(t *testing.T) {
	f := newFixture(t)
	name := "conflict\nname.txt"
	write(t, f.seed, name, "original\n")
	git(t, f.seed, "add", ".")
	git(t, f.seed, "commit", "-m", "common file")
	git(t, f.seed, "push", "origin", "main")
	run := prepare(t, f)
	write(t, run.Path, name, "feature\n")
	git(t, run.Path, "add", ".")
	git(t, run.Path, "commit", "-m", "feature")
	head := git(t, run.Path, "rev-parse", "HEAD")
	write(t, f.seed, name, "base\n")
	git(t, f.seed, "add", ".")
	git(t, f.seed, "commit", "-m", "base")
	git(t, f.seed, "push", "origin", "main")
	merge, err := f.manager.CheckBase(context.Background(), run, "main", head)
	if err != nil {
		t.Fatal(err)
	}
	if len(merge.Conflicts) != 1 || merge.Conflicts[0] != name {
		t.Fatalf("paths=%q", merge.Conflicts)
	}
	if _, err := f.manager.VerificationCommit(context.Background(), run, merge); err == nil {
		t.Fatal("created conflicted verification")
	}
	if got := git(t, run.Path, "status", "--porcelain"); got != "" {
		t.Fatalf("run workspace changed: %s", got)
	}
}
