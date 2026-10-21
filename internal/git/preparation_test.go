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
