package git_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestDiscardPreparationReleasesTheLeftoversOfACompletedCreation proves that
// a preparation whose ownership was never saved releases the branch and the
// worktree it created, so the next run for the same issue can prepare.
func TestDiscardPreparationReleasesTheLeftoversOfACompletedCreation(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	intent := prepareIntentPath(t, f)
	leftovers, err := f.manager.DiscardPreparation(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	if leftovers.Preserved {
		t.Fatalf("untouched leftovers were preserved: %+v", leftovers)
	}
	if _, err := os.Stat(run.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the worktree survived the discard: %v", err)
	}
	if branch := git(t, run.BasePath, "branch", "--list", run.Branch); branch != "" {
		t.Fatalf("the branch survived the discard: %s", branch)
	}
	if _, err := os.Stat(intent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the preparation claim survived the discard: %v", err)
	}
	// The next run for the same issue prepares without a branch conflict.
	f.request.RunID = "run-2"
	next := prepare(t, f)
	if next.Branch != run.Branch {
		t.Fatalf("the next run used another branch: %+v", next)
	}
}

// TestDiscardPreparationReleasesAnInterruptedCreation proves that a creation
// that never finished leaves nothing behind: neither the partial worktree nor
// the branch it created.
func TestDiscardPreparationReleasesAnInterruptedCreation(t *testing.T) {
	f := newFixture(t)
	path := interruptWorktreeCreation(t, f)
	leftovers, err := f.manager.DiscardPreparation(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	if leftovers.Preserved {
		t.Fatalf("the interrupted creation was preserved: %+v", leftovers)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the interrupted worktree survived the discard: %v", err)
	}
	base := filepath.Join(f.root, "repos", "owner-repo", "base")
	if branch := git(t, base, "branch", "--list", "mergeyard/issue-7"); branch != "" {
		t.Fatalf("the interrupted branch survived the discard: %s", branch)
	}
}

// TestDiscardPreparationPreservesLeftoversWithChanges proves that leftovers
// holding changes are never deleted: they are reported instead.
func TestDiscardPreparationPreservesLeftoversWithChanges(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	write(t, run.Path, "human.txt", "uncommitted work\n")
	leftovers, err := f.manager.DiscardPreparation(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	if !leftovers.Preserved || leftovers.Branch != run.Branch || leftovers.Path != run.Path || leftovers.Reason == "" {
		t.Fatalf("the changed leftovers were not reported: %+v", leftovers)
	}
	if data, err := os.ReadFile(filepath.Join(run.Path, "human.txt")); err != nil || string(data) != "uncommitted work\n" {
		t.Fatalf("the changed worktree was discarded: %q, %v", data, err)
	}
	if branch := git(t, run.BasePath, "branch", "--list", run.Branch); branch == "" {
		t.Fatal("the branch of the changed worktree was deleted")
	}
}

// TestDiscardPreparationPreservesLeftoversWithCommits proves that leftovers
// holding commits are never deleted: they are reported instead.
func TestDiscardPreparationPreservesLeftoversWithCommits(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	write(t, run.Path, "feature.txt", "work\n")
	git(t, run.Path, "add", ".")
	git(t, run.Path, "commit", "-m", "agent work")
	leftovers, err := f.manager.DiscardPreparation(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	if !leftovers.Preserved || leftovers.Branch != run.Branch || leftovers.Reason == "" {
		t.Fatalf("the committed leftovers were not reported: %+v", leftovers)
	}
	if got := git(t, run.Path, "log", "--oneline"); got == "" {
		t.Fatal("the commit was discarded")
	}
	if branch := git(t, run.BasePath, "branch", "--list", run.Branch); branch == "" {
		t.Fatal("the branch of the committed worktree was deleted")
	}
}

// TestDiscardPreparationLeavesAnotherRunsArtifactsAlone proves that a run
// without a preparation claim never removes what another run created.
func TestDiscardPreparationLeavesAnotherRunsArtifactsAlone(t *testing.T) {
	f := newFixture(t)
	other := f.request
	other.RunID, other.IssueNumber = "other", 8
	run, err := f.manager.Prepare(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	leftovers, err := f.manager.DiscardPreparation(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	if leftovers.Preserved || leftovers.Branch != "" || leftovers.Path != "" {
		t.Fatalf("a run without a claim reported leftovers: %+v", leftovers)
	}
	if _, err := os.Stat(run.Path); err != nil {
		t.Fatalf("another run's worktree was removed: %v", err)
	}
	if branch := git(t, run.BasePath, "branch", "--list", run.Branch); branch == "" {
		t.Fatal("another run's branch was deleted")
	}
}
