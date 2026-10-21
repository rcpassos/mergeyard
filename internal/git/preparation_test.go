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
	seedSubmoduleWithIgnore(t, f, "")
}

// seedSubmoduleWithIgnore adds a submodule whose .gitmodules entry carries an
// ignore setting, so the fixture reproduces a repository that hides submodule
// changes from status.
func seedSubmoduleWithIgnore(t *testing.T, f fixture, ignore string) {
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
	if ignore != "" {
		git(t, f.seed, "config", "-f", ".gitmodules", "submodule.module.ignore", ignore)
		git(t, f.seed, "add", ".gitmodules")
	}
	git(t, f.seed, "commit", "-m", "add submodule")
	git(t, f.seed, "push", "origin", "main")
}

// seedNestedSubmodule adds a submodule whose own recorded tree holds another
// submodule, so the nested directory can be inspected recursively.
func seedNestedSubmodule(t *testing.T, f fixture) {
	t.Helper()
	root := t.TempDir()
	nestedRemote := filepath.Join(root, "nested.git")
	nestedSeed := filepath.Join(root, "nested-seed")
	git(t, root, "init", "--bare", "--initial-branch=main", nestedRemote)
	git(t, root, "init", "--initial-branch=main", nestedSeed)
	write(t, nestedSeed, "nested.txt", "nested\n")
	git(t, nestedSeed, "add", ".")
	git(t, nestedSeed, "commit", "-m", "nested")
	git(t, nestedSeed, "remote", "add", "origin", nestedRemote)
	git(t, nestedSeed, "push", "origin", "main")

	moduleRemote := filepath.Join(root, "module.git")
	moduleSeed := filepath.Join(root, "module-seed")
	git(t, root, "init", "--bare", "--initial-branch=main", moduleRemote)
	git(t, root, "init", "--initial-branch=main", moduleSeed)
	write(t, moduleSeed, "module.txt", "module\n")
	git(t, moduleSeed, "add", ".")
	git(t, moduleSeed, "commit", "-m", "module")
	git(t, moduleSeed, "-c", "protocol.file.allow=always", "submodule", "add", "--", nestedRemote, "nested")
	git(t, moduleSeed, "commit", "-m", "add nested submodule")
	git(t, moduleSeed, "remote", "add", "origin", moduleRemote)
	git(t, moduleSeed, "push", "origin", "main")

	git(t, f.seed, "-c", "protocol.file.allow=always", "submodule", "add", "--", moduleRemote, "module")
	git(t, f.seed, "commit", "-m", "add submodule")
	git(t, f.seed, "push", "origin", "main")
}

// lockInitialization reinstates the lock Git holds while it initializes a
// worktree, so the leftover is checked like an unfinished checkout.
func lockInitialization(t *testing.T, run managedgit.Run) {
	t.Helper()
	lock := filepath.Join(run.BasePath, ".git", "worktrees", run.ID, "locked")
	if err := os.WriteFile(lock, []byte("initializing\n"), 0600); err != nil {
		t.Fatal(err)
	}
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

// TestDiscardPreparationPreservesHumanWorkInAnUninitializedSubmoduleOfACheckout
// proves that the clean, unlocked leftovers of an interrupted preparation are
// inspected for content inside an uninitialized submodule, which Git's status
// does not see.
func TestDiscardPreparationPreservesHumanWorkInAnUninitializedSubmoduleOfACheckout(t *testing.T) {
	f := newFixture(t)
	seedSubmodule(t, f)
	path := interruptWorktreeCreation(t, f)
	write(t, filepath.Join(path, "module"), "human.txt", "uncommitted human work\n")
	if status := git(t, path, "status", "--porcelain", "--untracked-files=all", "--ignored"); status != "" {
		t.Fatalf("the fixture file is not hidden from status: %q", status)
	}
	leftovers, err := f.manager.DiscardPreparation(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	if !leftovers.Preserved || leftovers.Unverified || leftovers.Reason == "" {
		t.Fatalf("the human work in the submodule was not reported: %+v", leftovers)
	}
	if data, err := os.ReadFile(filepath.Join(path, "module", "human.txt")); err != nil || string(data) != "uncommitted human work\n" {
		t.Fatalf("the human work was deleted: %q, %v", data, err)
	}
	if branch := git(t, filepath.Join(f.root, "repos", "owner-repo", "base"), "branch", "--list", "mergeyard/issue-7"); branch == "" {
		t.Fatal("the branch of the preserved work was deleted")
	}
}

// TestDiscardPreparationPreservesWorkInAnInitializedSubmoduleHiddenByIgnore
// proves that .gitmodules ignore settings cannot hide a person's work inside an
// initialized submodule of a clean, unlocked leftover checkout.
func TestDiscardPreparationPreservesWorkInAnInitializedSubmoduleHiddenByIgnore(t *testing.T) {
	for _, tc := range []struct {
		ignore, name, file, content string
		commit                      bool
	}{
		{"dirty", "edited tracked file", "module.txt", "edited by a person\n", false},
		{"untracked", "untracked file", "human.txt", "uncommitted human work\n", false},
		{"all", "edited tracked file", "module.txt", "edited by a person\n", false},
		{"all", "commit", "module.txt", "committed by a person\n", true},
	} {
		t.Run(tc.ignore+"/"+tc.name, func(t *testing.T) {
			f := newFixture(t)
			seedSubmoduleWithIgnore(t, f, tc.ignore)
			path := interruptWorktreeCreation(t, f)
			submodule := filepath.Join(path, "module")
			git(t, path, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--", "module")
			write(t, submodule, tc.file, tc.content)
			if tc.commit {
				git(t, submodule, "add", tc.file)
				git(t, submodule, "commit", "-m", "person work")
			}
			if status := git(t, path, "status", "--porcelain", "--untracked-files=all", "--ignored"); status != "" {
				t.Fatalf("the fixture work is not hidden from status: %q", status)
			}
			leftovers, err := f.manager.DiscardPreparation(context.Background(), f.request)
			if err != nil {
				t.Fatal(err)
			}
			if !leftovers.Preserved || leftovers.Unverified || leftovers.Reason == "" {
				t.Fatalf("the work in the submodule was not reported: %+v", leftovers)
			}
			if data, err := os.ReadFile(filepath.Join(path, "module", tc.file)); err != nil || string(data) != tc.content {
				t.Fatalf("the work was deleted: %q, %v", data, err)
			}
		})
	}
}

// TestDiscardPreparationReleasesACleanInitializedSubmodule proves that an
// initialized submodule holding exactly the recorded start is not mistaken for
// work, so the release can still discard the leftover.
func TestDiscardPreparationReleasesACleanInitializedSubmodule(t *testing.T) {
	f := newFixture(t)
	seedSubmodule(t, f)
	path := interruptWorktreeCreation(t, f)
	git(t, path, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--", "module")
	if status := git(t, path, "status", "--porcelain", "--untracked-files=all", "--ignored", "--ignore-submodules=none"); status != "" {
		t.Fatalf("the initialized submodule is not clean: %q", status)
	}
	leftovers, err := f.manager.DiscardPreparation(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	if leftovers.Preserved {
		t.Fatalf("the clean initialized submodule was preserved: %+v", leftovers)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the leftover survived the release: %v", err)
	}
	base := filepath.Join(f.root, "repos", "owner-repo", "base")
	if branch := git(t, base, "branch", "--list", "mergeyard/issue-7"); branch != "" {
		t.Fatalf("the branch survived the release: %s", branch)
	}
}

// TestDiscardPreparationPreservesAHiddenEditInAnInitializingSubmodule proves
// that index flags cannot hide a tracked edit inside an initialized submodule
// of a leftover checkout Git still holds locked for initialization.
func TestDiscardPreparationPreservesAHiddenEditInAnInitializingSubmodule(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		t.Run(flag, func(t *testing.T) {
			f := newFixture(t)
			seedSubmodule(t, f)
			run := prepare(t, f)
			submodule := filepath.Join(run.Path, "module")
			git(t, run.Path, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--", "module")
			git(t, submodule, "update-index", flag, "module.txt")
			write(t, submodule, "module.txt", "edited by a person\n")
			if status := git(t, submodule, "status", "--porcelain", "--untracked-files=all", "--ignored"); status != "" {
				t.Fatalf("the fixture edit is not hidden from status: %q", status)
			}
			lockInitialization(t, run)
			leftovers, err := f.manager.DiscardPreparation(context.Background(), f.request)
			if err != nil {
				t.Fatal(err)
			}
			if !leftovers.Preserved || leftovers.Unverified || leftovers.Reason == "" {
				t.Fatalf("the hidden edit was not reported: %+v", leftovers)
			}
			if data, err := os.ReadFile(filepath.Join(submodule, "module.txt")); err != nil || string(data) != "edited by a person\n" {
				t.Fatalf("the hidden edit was deleted: %q, %v", data, err)
			}
		})
	}
}

// TestDiscardPreparationPreservesASubmoduleStashInAnInitializingCheckout proves
// that a clean submodule checkout does not prove its repository holds no saved
// work: a stash must survive, because removal deletes the submodule's Git
// repository with it.
func TestDiscardPreparationPreservesASubmoduleStashInAnInitializingCheckout(t *testing.T) {
	f := newFixture(t)
	seedSubmodule(t, f)
	run := prepare(t, f)
	submodule := filepath.Join(run.Path, "module")
	git(t, run.Path, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--", "module")
	write(t, submodule, "module.txt", "saved by a person\n")
	git(t, submodule, "stash", "push", "-m", "person work")
	if stash := git(t, submodule, "stash", "list"); stash == "" {
		t.Fatal("the fixture stash was not created")
	}
	lockInitialization(t, run)
	leftovers, err := f.manager.DiscardPreparation(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	if !leftovers.Preserved || leftovers.Unverified || leftovers.Reason == "" {
		t.Fatalf("the stashed work was not reported: %+v", leftovers)
	}
	if stash := git(t, submodule, "stash", "list"); stash == "" {
		t.Fatal("the stashed work was deleted")
	}
}

// TestDiscardPreparationReleasesACleanInitializedSubmoduleInAnInitializingCheckout
// proves that the extra checks do not preserve a clean initialized submodule
// that holds nothing beyond its recorded commit and has no saved repository
// data.
func TestDiscardPreparationReleasesACleanInitializedSubmoduleInAnInitializingCheckout(t *testing.T) {
	f := newFixture(t)
	seedSubmodule(t, f)
	run := prepare(t, f)
	submodule := filepath.Join(run.Path, "module")
	git(t, run.Path, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--", "module")
	if status := git(t, submodule, "status", "--porcelain", "--untracked-files=all", "--ignored"); status != "" {
		t.Fatalf("the initialized submodule is not clean: %q", status)
	}
	lockInitialization(t, run)
	leftovers, err := f.manager.DiscardPreparation(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	if leftovers.Preserved {
		t.Fatalf("the clean initialized submodule was preserved: %+v", leftovers)
	}
	if _, err := os.Stat(run.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the leftover survived the release: %v", err)
	}
}

// TestPreparePreservesHumanWorkInAnUninitializedSubmoduleOfAnInterruptedCheckout
// proves that a retried Prepare keeps a clean, unlocked leftover checkout that
// holds a file in an uninitialized submodule, and reports the branch conflict
// that names it.
func TestPreparePreservesHumanWorkInAnUninitializedSubmoduleOfAnInterruptedCheckout(t *testing.T) {
	f := newFixture(t)
	seedSubmodule(t, f)
	path := interruptWorktreeCreation(t, f)
	write(t, filepath.Join(path, "module"), "human.txt", "uncommitted human work\n")
	if status := git(t, path, "status", "--porcelain", "--untracked-files=all", "--ignored"); status != "" {
		t.Fatalf("the fixture file is not hidden from status: %q", status)
	}
	_, err := f.manager.Prepare(context.Background(), f.request)
	requireConflictNaming(t, err, path)
	if data, err := os.ReadFile(filepath.Join(path, "module", "human.txt")); err != nil || string(data) != "uncommitted human work\n" {
		t.Fatalf("the human work was deleted: %q, %v", data, err)
	}
}

// TestPreparePreservesWorkInAnInitializedSubmoduleHiddenByIgnore proves that a
// retried Prepare does not let .gitmodules ignore settings hide a person's work
// inside an initialized submodule of an interrupted checkout.
func TestPreparePreservesWorkInAnInitializedSubmoduleHiddenByIgnore(t *testing.T) {
	for _, tc := range []struct {
		ignore, name, file, content string
		commit                      bool
	}{
		{"dirty", "edited tracked file", "module.txt", "edited by a person\n", false},
		{"untracked", "untracked file", "human.txt", "uncommitted human work\n", false},
		{"all", "edited tracked file", "module.txt", "edited by a person\n", false},
		{"all", "commit", "module.txt", "committed by a person\n", true},
	} {
		t.Run(tc.ignore+"/"+tc.name, func(t *testing.T) {
			f := newFixture(t)
			seedSubmoduleWithIgnore(t, f, tc.ignore)
			path := interruptWorktreeCreation(t, f)
			submodule := filepath.Join(path, "module")
			git(t, path, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--", "module")
			write(t, submodule, tc.file, tc.content)
			if tc.commit {
				git(t, submodule, "add", tc.file)
				git(t, submodule, "commit", "-m", "person work")
			}
			if status := git(t, path, "status", "--porcelain", "--untracked-files=all", "--ignored"); status != "" {
				t.Fatalf("the fixture work is not hidden from status: %q", status)
			}
			_, err := f.manager.Prepare(context.Background(), f.request)
			requireConflictNaming(t, err, path)
			if data, err := os.ReadFile(filepath.Join(path, "module", tc.file)); err != nil || string(data) != tc.content {
				t.Fatalf("the work was deleted: %q, %v", data, err)
			}
		})
	}
}

// TestPrepareRecreatesAnInterruptedCheckoutWithEmptySubmoduleDirectories proves
// that empty submodule directories are not mistaken for work: a retried Prepare
// discards the leftover and creates it again.
func TestPrepareRecreatesAnInterruptedCheckoutWithEmptySubmoduleDirectories(t *testing.T) {
	f := newFixture(t)
	seedSubmodule(t, f)
	path := interruptWorktreeCreation(t, f)
	entries, err := os.ReadDir(filepath.Join(path, "module"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("the interrupted checkout does not hold an empty submodule: %v, %v", entries, err)
	}
	run := prepare(t, f)
	if run.Path != path {
		t.Fatalf("the leftover was not recreated for the run: %+v", run)
	}
	if got := git(t, run.Path, "branch", "--show-current"); got != run.Branch {
		t.Fatalf("recreated worktree branch = %q", got)
	}
	if got := git(t, run.Path, "rev-parse", "HEAD"); got != run.BaseSHA {
		t.Fatalf("recreated worktree started at %s, want %s", got, run.BaseSHA)
	}
	entries, err = os.ReadDir(filepath.Join(run.Path, "module"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("the recreated checkout does not hold an empty submodule: %v, %v", entries, err)
	}
}

// TestPrepareRecreatesAnInterruptedCheckoutWithACleanInitializedSubmodule
// proves that an initialized submodule holding exactly the recorded start is
// not mistaken for work, so a retried Prepare discards and recreates it.
func TestPrepareRecreatesAnInterruptedCheckoutWithACleanInitializedSubmodule(t *testing.T) {
	f := newFixture(t)
	seedSubmodule(t, f)
	path := interruptWorktreeCreation(t, f)
	git(t, path, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--", "module")
	if status := git(t, path, "status", "--porcelain", "--untracked-files=all", "--ignored", "--ignore-submodules=none"); status != "" {
		t.Fatalf("the initialized submodule is not clean: %q", status)
	}
	run := prepare(t, f)
	if run.Path != path {
		t.Fatalf("the leftover was not recreated for the run: %+v", run)
	}
	if got := git(t, run.Path, "branch", "--show-current"); got != run.Branch {
		t.Fatalf("recreated worktree branch = %q", got)
	}
	if got := git(t, run.Path, "rev-parse", "HEAD"); got != run.BaseSHA {
		t.Fatalf("recreated worktree started at %s, want %s", got, run.BaseSHA)
	}
}

// TestPreparePreservesHumanWorkInAnUninitializedNestedSubmodule proves that the
// submodule inspection recurses: a file inside a nested submodule that was
// never initialized is invisible even to a status check that overrides
// submodule ignore settings.
func TestPreparePreservesHumanWorkInAnUninitializedNestedSubmodule(t *testing.T) {
	f := newFixture(t)
	seedNestedSubmodule(t, f)
	path := interruptWorktreeCreation(t, f)
	git(t, path, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--", "module")
	write(t, filepath.Join(path, "module", "nested"), "human.txt", "uncommitted human work\n")
	if status := git(t, path, "status", "--porcelain", "--untracked-files=all", "--ignored", "--ignore-submodules=none"); status != "" {
		t.Fatalf("the fixture file is not hidden from status: %q", status)
	}
	_, err := f.manager.Prepare(context.Background(), f.request)
	requireConflictNaming(t, err, path)
	if data, err := os.ReadFile(filepath.Join(path, "module", "nested", "human.txt")); err != nil || string(data) != "uncommitted human work\n" {
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

// TestDiscardPreparationPreservesLeftoversWhenTheBaseIsGone proves that a claim
// that outlives its managed base does not make the release fail: the leftovers
// cannot be proven empty, so they are reported instead and stopping goes on.
func TestDiscardPreparationPreservesLeftoversWhenTheBaseIsGone(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	if err := os.RemoveAll(run.BasePath); err != nil {
		t.Fatal(err)
	}
	leftovers, err := f.manager.DiscardPreparation(context.Background(), f.request)
	if err != nil {
		t.Fatalf("a missing base failed the release: %v", err)
	}
	requireUnverified(t, f, run, leftovers)
}

// requireUnverified proves that leftovers the release could not check are
// reported as unverified under the run's names, and that nothing was deleted:
// the worktree, the branch where its base still has it, and the claim.
func requireUnverified(t *testing.T, f fixture, run managedgit.Run, leftovers managedgit.Leftovers) {
	t.Helper()
	if !leftovers.Preserved || !leftovers.Unverified || leftovers.Branch != run.Branch || leftovers.Path != run.Path || leftovers.Reason == "" {
		t.Fatalf("the unverifiable leftovers were not reported: %+v", leftovers)
	}
	if _, err := os.Stat(run.Path); err != nil {
		t.Fatalf("the unverified worktree was removed: %v", err)
	}
	if _, err := os.Stat(prepareIntentPath(t, f)); err != nil {
		t.Fatalf("the claim was released with leftovers still in place: %v", err)
	}
}

// TestDiscardPreparationPreservesLeftoversWhenTheClaimIsUnreadable proves that
// an empty or corrupt claim, which a crash or a person can leave, does not make
// the release fail: the leftovers cannot be proven to be this run's, so the
// branch and the worktree preparation creates for the run are reported instead.
func TestDiscardPreparationPreservesLeftoversWhenTheClaimIsUnreadable(t *testing.T) {
	for name, content := range map[string]string{"empty": "", "corrupt": `{"id":"run-1","branch":`, "foreign": `{}`} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			run := prepare(t, f)
			if err := os.WriteFile(prepareIntentPath(t, f), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			leftovers, err := f.manager.DiscardPreparation(context.Background(), f.request)
			if err != nil {
				t.Fatalf("an unreadable claim failed the release: %v", err)
			}
			requireUnverified(t, f, run, leftovers)
			if branch := git(t, run.BasePath, "branch", "--list", run.Branch); branch == "" {
				t.Fatal("the branch was deleted under an unreadable claim")
			}
		})
	}
}

// TestDiscardPreparationPreservesLeftoversWhenTheBaseIsReplaced proves that a
// managed base replaced by another clone does not make the release fail: the
// leftovers are attached to a base that is gone, so they are reported as
// unverified instead of removed.
func TestDiscardPreparationPreservesLeftoversWhenTheBaseIsReplaced(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	if err := os.RemoveAll(run.BasePath); err != nil {
		t.Fatal(err)
	}
	git(t, f.root, "clone", "--quiet", f.remote, run.BasePath)
	leftovers, err := f.manager.DiscardPreparation(context.Background(), f.request)
	if err != nil {
		t.Fatalf("a replaced base failed the release: %v", err)
	}
	requireUnverified(t, f, run, leftovers)
}

// TestDiscardPreparationReportsAnotherBranchAsWork proves that a checkout a
// person moved to another branch is work the release found, not leftovers it
// could not check.
func TestDiscardPreparationReportsAnotherBranchAsWork(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	git(t, run.Path, "checkout", "--quiet", "-b", "person/topic")
	leftovers, err := f.manager.DiscardPreparation(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	if !leftovers.Preserved || leftovers.Unverified || leftovers.Reason == "" {
		t.Fatalf("the other branch was not reported as work: %+v", leftovers)
	}
	if got := git(t, run.Path, "branch", "--show-current"); got != "person/topic" {
		t.Fatalf("the other checkout was changed: %q", got)
	}
}

// TestDiscardPreparationPreservesLeftoversWhenGitFails proves that a Git check
// that fails or exceeds its deadline does not make the release fail, so a stop
// cannot wedge on it, and that the leftovers stay claimed for a later release
// once Git answers again.
func TestDiscardPreparationPreservesLeftoversWhenGitFails(t *testing.T) {
	for name, body := range map[string]string{"failing": "exit 128", "hung": "exec /bin/sleep 600"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			run := prepare(t, f)
			installGitWrapper(t, "status", body)
			manager := managerWithTimeouts(t, f, shortTimeouts(time.Second))
			leftovers, err := manager.DiscardPreparation(context.Background(), f.request)
			if err != nil {
				t.Fatalf("a Git failure failed the release: %v", err)
			}
			requireUnverified(t, f, run, leftovers)
		})
	}
}

// TestDiscardPreparationReportsACancelledCaller proves that a caller that gave
// up gets an error instead of a verdict, so the stop is attempted again rather
// than finished on a check that never ran.
func TestDiscardPreparationReportsACancelledCaller(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.manager.DiscardPreparation(ctx, f.request); err == nil {
		t.Fatal("a cancelled caller got a verdict")
	}
	if _, err := os.Stat(run.Path); err != nil {
		t.Fatalf("a cancelled release removed the worktree: %v", err)
	}
}

// TestDiscardPreparationWithoutAClaimDoesNotWaitForTheGitLock proves that a run
// that never started preparing returns at once while another operation holds
// the manager's lock.
func TestDiscardPreparationWithoutAClaimDoesNotWaitForTheGitLock(t *testing.T) {
	f := newFixture(t)
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
    /bin/sleep 10
  fi
done
`, marker)
	if err := os.MkdirAll(filepath.Dir(hook), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	busy, cancelBusy := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() {
		cancelBusy()
		<-done
	})
	go func() {
		defer close(done)
		_, _ = f.manager.Prepare(busy, f.request)
	}()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the busy operation never took the lock")
		}
		time.Sleep(50 * time.Millisecond)
	}
	idle := f.request
	idle.RunID, idle.IssueNumber = "idle", 8
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	leftovers, err := f.manager.DiscardPreparation(ctx, idle)
	if err != nil {
		t.Fatalf("a run without a claim waited for the lock: %v", err)
	}
	if leftovers.Preserved {
		t.Fatalf("a run without a claim reported leftovers: %+v", leftovers)
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
