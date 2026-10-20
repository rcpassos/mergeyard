package git_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	managedgit "github.com/rcpassos/mergeyard/internal/git"
)

// interruptBeforeHead reproduces a creation that Git never finished: the
// working tree is written, but HEAD is still pending and the checkout stays
// locked for initialization.
func interruptBeforeHead(t *testing.T, f fixture) string {
	t.Helper()
	setup := f.request
	setup.RunID, setup.IssueNumber = "setup", 6
	if _, err := f.manager.Prepare(context.Background(), setup); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(f.root, "repos", "owner-repo", "base", ".git", "hooks", "reference-transaction")
	marker := filepath.Join(t.TempDir(), "head-prepared")
	script := fmt.Sprintf(`#!/bin/sh
[ "$1" = prepared ] || exit 0
while read old new ref; do
  if [ "$ref" = HEAD ] && [ "$new" = ref:refs/heads/mergeyard/issue-7 ]; then
    : > '%s'
    /bin/sleep 30
  fi
done
`, marker)
	if err := os.MkdirAll(filepath.Dir(hook), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := f.manager.Prepare(ctx, f.request)
	requireCode(t, err, "git.timeout")
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	path, err := managedgit.WorktreePath(f.root, f.request.Repository, f.request.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the interrupted creation left no worktree: %v", err)
	}
	return path
}

// TestDiscardPreparationPreservesHumanWorkInAnUnfinishedInitialization proves
// that a directory Git never finished initializing is not removed while it
// holds files a person added.
func TestDiscardPreparationPreservesHumanWorkInAnUnfinishedInitialization(t *testing.T) {
	f := newFixture(t)
	path := interruptBeforeHead(t, f)
	write(t, path, "human.txt", "uncommitted human work\n")
	leftovers, err := f.manager.DiscardPreparation(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	if !leftovers.Preserved || leftovers.Reason == "" {
		t.Fatalf("the human work was not reported: %+v", leftovers)
	}
	if data, err := os.ReadFile(filepath.Join(path, "human.txt")); err != nil || string(data) != "uncommitted human work\n" {
		t.Fatalf("the human work was deleted: %q, %v", data, err)
	}
	base := filepath.Join(f.root, "repos", "owner-repo", "base")
	if branch := git(t, base, "branch", "--list", "mergeyard/issue-7"); branch == "" {
		t.Fatal("the branch of the preserved work was deleted")
	}
}

// TestDiscardPreparationReleasesAnUnfinishedInitialization proves that a
// directory Git never finished initializing is released when it holds exactly
// the recorded start and nothing else.
func TestDiscardPreparationReleasesAnUnfinishedInitialization(t *testing.T) {
	f := newFixture(t)
	path := interruptBeforeHead(t, f)
	leftovers, err := f.manager.DiscardPreparation(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	if leftovers.Preserved {
		t.Fatalf("the untouched initialization was preserved: %+v", leftovers)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the initialization survived the discard: %v", err)
	}
	base := filepath.Join(f.root, "repos", "owner-repo", "base")
	if branch := git(t, base, "branch", "--list", "mergeyard/issue-7"); branch != "" {
		t.Fatalf("the branch survived the discard: %s", branch)
	}
}

// seedSubmodule adds a submodule to the fixture's seed repository, so the
// recorded tree holds a gitlink.
func seedSubmodule(t *testing.T, f fixture) {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "module.git")
	seed := filepath.Join(root, "module-seed")
	git(t, root, "init", "--bare", "--initial-branch=main", remote)
	git(t, root, "init", "--initial-branch=main", seed)
	write(t, seed, "module.txt", "module\n")
	git(t, seed, "add", ".")
	git(t, seed, "commit", "-m", "module")
	git(t, seed, "remote", "add", "origin", remote)
	git(t, seed, "push", "origin", "main")
	git(t, f.seed, "-c", "protocol.file.allow=always", "submodule", "add", "--", remote, "module")
	git(t, f.seed, "commit", "-m", "add submodule")
	git(t, f.seed, "push", "origin", "main")
}

// TestDiscardPreparationPreservesHumanWorkInAnUninitializedSubmodule proves
// that a submodule of the recorded tree is inspected: Git compares a submodule
// by the commit it points at and never by the files inside it, so a person can
// add files to an uninitialized submodule that no other check would see.
func TestDiscardPreparationPreservesHumanWorkInAnUninitializedSubmodule(t *testing.T) {
	f := newFixture(t)
	seedSubmodule(t, f)
	path := interruptBeforeHead(t, f)
	if err := os.MkdirAll(filepath.Join(path, "module"), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(path, "module"), "human.txt", "uncommitted human work\n")
	leftovers, err := f.manager.DiscardPreparation(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	if !leftovers.Preserved || leftovers.Reason == "" {
		t.Fatalf("the human work in the submodule was not reported: %+v", leftovers)
	}
	if data, err := os.ReadFile(filepath.Join(path, "module", "human.txt")); err != nil || string(data) != "uncommitted human work\n" {
		t.Fatalf("the human work was deleted: %q, %v", data, err)
	}
}

// TestPrepareBranchConflictKeepsTheMetadataDeadline proves that the lookup of
// the checkout that holds a conflicting branch keeps the metadata deadline, so
// a stalled lookup cannot hold the preparation open for its whole budget.
func TestPrepareBranchConflictKeepsTheMetadataDeadline(t *testing.T) {
	f := newFixture(t)
	prepare(t, f)
	// The lookup of the worktrees that hold the branch stalls.
	installGitWrapper(t, "worktree list", "/bin/sleep 30")
	f.request.RunID = "run-2"
	manager := managerWithTimeouts(t, f, managedgit.Timeouts{Metadata: time.Second, Worktree: 5 * time.Second, Clone: 5 * time.Second, Fetch: 5 * time.Second, Push: 5 * time.Second})
	_, err := manager.Prepare(context.Background(), f.request)
	requireCode(t, err, "git.timeout")
}

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

// TestDiscardPreparationPreservesIgnoredFilesInACheckout proves that a file the
// repository ignores is still a person's work: it does not show in the plain
// status of a checkout, so the release must look for it explicitly.
func TestDiscardPreparationPreservesIgnoredFilesInACheckout(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	write(t, run.Path, "local.ignored", "local data\n")
	if status := git(t, run.Path, "status", "--porcelain"); status != "" {
		t.Fatalf("the fixture file is not ignored: %q", status)
	}
	leftovers, err := f.manager.DiscardPreparation(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	if !leftovers.Preserved || leftovers.Reason == "" {
		t.Fatalf("the ignored file was not reported: %+v", leftovers)
	}
	if data, err := os.ReadFile(filepath.Join(run.Path, "local.ignored")); err != nil || string(data) != "local data\n" {
		t.Fatalf("the ignored file was deleted: %q, %v", data, err)
	}
	if branch := git(t, run.BasePath, "branch", "--list", run.Branch); branch == "" {
		t.Fatal("the branch of the preserved worktree was deleted")
	}
}

// TestDiscardPreparationPreservesIgnoredFilesInAnUnfinishedInitialization
// proves the same for a directory Git never finished initializing.
func TestDiscardPreparationPreservesIgnoredFilesInAnUnfinishedInitialization(t *testing.T) {
	f := newFixture(t)
	path := interruptBeforeHead(t, f)
	// An exclude rule of the managed base hides the file from Git's ignore
	// listing without a tracked .gitignore.
	exclude := filepath.Join(f.root, "repos", "owner-repo", "base", ".git", "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(exclude), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exclude, []byte(".env\n"), 0600); err != nil {
		t.Fatal(err)
	}
	write(t, path, ".env", "local data\n")
	leftovers, err := f.manager.DiscardPreparation(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	if !leftovers.Preserved || leftovers.Reason == "" {
		t.Fatalf("the ignored file was not reported: %+v", leftovers)
	}
	if data, err := os.ReadFile(filepath.Join(path, ".env")); err != nil || string(data) != "local data\n" {
		t.Fatalf("the ignored file was deleted: %q, %v", data, err)
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
