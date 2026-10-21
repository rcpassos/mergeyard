package git_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/workspace"
)

type fixture struct {
	manager *managedgit.Manager
	request managedgit.PrepareRequest
	seed    string
	remote  string
	root    string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	// Use local repositories and a deterministic identity; never read or change
	// the user's Git identity, signing configuration, hooks, or remote.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "Mergeyard Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "mergeyard@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "Mergeyard Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "mergeyard@example.invalid")
	root := t.TempDir()
	seed := filepath.Join(root, "seed")
	remote := filepath.Join(root, "remote.git")
	git(t, root, "init", "--bare", "--initial-branch=main", remote)
	git(t, root, "init", "--initial-branch=main", seed)
	write(t, seed, "README.md", "initial\n")
	write(t, seed, ".gitignore", "*.ignored\n")
	git(t, seed, "add", ".")
	git(t, seed, "commit", "-m", "initial")
	git(t, seed, "remote", "add", "origin", remote)
	git(t, seed, "push", "origin", "main")
	w, err := workspace.Open(filepath.Join(root, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	return fixture{
		manager: managedgit.New(w), seed: seed, remote: remote, root: w.Root,
		request: managedgit.PrepareRequest{Repository: "owner/repo", RemoteURL: remote, RunID: "run-1", IssueNumber: 7},
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, output)
	}
	return strings.TrimSpace(string(output))
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var failure *fault.Error
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("expected %s, got %v", code, err)
	}
}

func prepare(t *testing.T, f fixture) managedgit.Run {
	t.Helper()
	run, err := f.manager.Prepare(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// installGitWrapper puts a shell wrapper first in PATH. A matching invocation
// runs body instead of Git and then delegates unless the body exits; every
// other invocation goes straight to the real Git resolved before PATH changed.
func installGitWrapper(t *testing.T, match, body string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is unavailable")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \" $* \" in\n*\" " + match + " \"*)\n" + body + "\n;;\nesac\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestPrepareClonesAndCreatesWorktreeFromRemoteDefault(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	if run.BasePath != filepath.Join(f.root, "repos", "owner-repo", "base") ||
		run.Path != filepath.Join(f.root, "worktrees", "owner-repo", "run-1") ||
		run.Branch != "mergeyard/issue-7" || run.BaseBranch != "main" {
		t.Fatalf("unexpected managed run: %+v", run)
	}
	if got := git(t, run.BasePath, "remote", "get-url", "origin"); got != f.remote {
		t.Fatalf("origin = %q", got)
	}
	if got := git(t, run.Path, "branch", "--show-current"); got != "mergeyard/issue-7" {
		t.Fatalf("worktree branch = %q", got)
	}
	if got := git(t, run.Path, "rev-parse", "HEAD"); got != git(t, f.seed, "rev-parse", "main") || got != run.BaseSHA {
		t.Fatalf("worktree did not start at fetched base: %s, %+v", got, run)
	}
}

func TestPrepareReusesBaseFetchesAndPrunes(t *testing.T) {
	f := newFixture(t)
	git(t, f.seed, "push", "origin", "main:refs/heads/stale")
	first := prepare(t, f)
	write(t, f.seed, "README.md", "updated upstream\n")
	git(t, f.seed, "commit", "-am", "upstream update")
	git(t, f.seed, "push", "origin", "main", ":refs/heads/stale")
	f.request.RunID, f.request.IssueNumber = "run-2", 8
	second := prepare(t, f)
	if second.BasePath != first.BasePath || second.BaseSHA != git(t, f.seed, "rev-parse", "main") {
		t.Fatalf("base was not reused and refreshed: %+v", second)
	}
	if git(t, first.Path, "rev-parse", "HEAD") != first.BaseSHA {
		t.Fatal("refresh moved the first run's worktree")
	}
	cmd := exec.Command("git", "show-ref", "--verify", "refs/remotes/origin/stale")
	cmd.Dir = second.BasePath
	if err := cmd.Run(); err == nil {
		t.Fatal("stale remote ref was not pruned")
	}
}

func TestPrepareRefusesDirtyBase(t *testing.T) {
	for _, name := range []string{"README.md", "untracked.txt"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			first := prepare(t, f)
			write(t, first.BasePath, name, "user changes\n")
			f.request.RunID, f.request.IssueNumber = "run-2", 8
			_, err := f.manager.Prepare(context.Background(), f.request)
			requireCode(t, err, "git.dirty_base")
			data, err := os.ReadFile(filepath.Join(first.BasePath, name))
			if err != nil || string(data) != "user changes\n" {
				t.Fatalf("dirty base was modified: %q, %v", data, err)
			}
		})
	}
}

func TestPrepareRefusesWrongOrigin(t *testing.T) {
	f := newFixture(t)
	first := prepare(t, f)
	git(t, first.BasePath, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "other.git"))
	f.request.RunID, f.request.IssueNumber = "run-2", 8
	_, err := f.manager.Prepare(context.Background(), f.request)
	requireCode(t, err, "git.origin_mismatch")
}

func TestPrepareUsesConfiguredBranch(t *testing.T) {
	f := newFixture(t)
	git(t, f.seed, "checkout", "-b", "release/next")
	write(t, f.seed, "README.md", "release\n")
	git(t, f.seed, "commit", "-am", "release")
	git(t, f.seed, "push", "origin", "release/next")
	f.request.BaseBranch = "release/next"
	run := prepare(t, f)
	if run.BaseBranch != "release/next" || run.BaseSHA != git(t, f.seed, "rev-parse", "HEAD") {
		t.Fatalf("configured base ignored: %+v", run)
	}
}

func TestPrepareRefusesMissingBaseBranch(t *testing.T) {
	f := newFixture(t)
	f.request.BaseBranch = "missing"
	_, err := f.manager.Prepare(context.Background(), f.request)
	requireCode(t, err, "git.base_branch")
}

func TestPrepareRefusesUnownedRemoteBranch(t *testing.T) {
	f := newFixture(t)
	git(t, f.seed, "checkout", "-b", "mergeyard/issue-7")
	write(t, f.seed, "README.md", "someone else's work\n")
	git(t, f.seed, "commit", "-am", "external work")
	git(t, f.seed, "push", "origin", "mergeyard/issue-7")
	want := git(t, f.remote, "rev-parse", "refs/heads/mergeyard/issue-7")
	_, err := f.manager.Prepare(context.Background(), f.request)
	requireCode(t, err, "git.branch_conflict")
	if got := git(t, f.remote, "rev-parse", "refs/heads/mergeyard/issue-7"); got != want {
		t.Fatal("unowned remote branch changed")
	}
}

func TestPrepareReconcilesOwnedRemoteBranch(t *testing.T) {
	f := newFixture(t)
	first := prepare(t, f)
	write(t, first.Path, "README.md", "prior run's work\n")
	git(t, first.Path, "commit", "-am", "agent commit")
	git(t, first.Path, "push", "origin", first.Branch)
	want := git(t, first.Path, "rev-parse", "HEAD")
	git(t, first.BasePath, "worktree", "remove", first.Path)
	git(t, first.BasePath, "branch", "-D", first.Branch)
	// Advancing the base must not change the saved comparison point on resume.
	write(t, f.seed, "README.md", "new base\n")
	git(t, f.seed, "commit", "-am", "new base")
	git(t, f.seed, "push", "origin", "main")
	f.request.KnownRuns = []managedgit.Run{first}
	resumed := prepare(t, f)
	if resumed.BaseSHA != first.BaseSHA || git(t, resumed.Path, "rev-parse", "HEAD") != want {
		t.Fatalf("resume lost prior work or original base: %+v", resumed)
	}
}

func TestPrepareResumesOwnedWorktreeWithoutDiscardingChanges(t *testing.T) {
	f := newFixture(t)
	first := prepare(t, f)
	write(t, first.Path, "partial.txt", "interrupted agent work\n")
	f.request.KnownRuns = []managedgit.Run{first}
	resumed := prepare(t, f)
	if !reflect.DeepEqual(resumed, first) {
		t.Fatalf("resume changed run data: %+v != %+v", resumed, first)
	}
	if data, err := os.ReadFile(filepath.Join(resumed.Path, "partial.txt")); err != nil || string(data) != "interrupted agent work\n" {
		t.Fatalf("resume discarded partial work: %q, %v", data, err)
	}
}

func TestPrepareCannotUseAnotherRunsOwnership(t *testing.T) {
	f := newFixture(t)
	first := prepare(t, f)
	git(t, first.Path, "push", "origin", first.Branch)
	f.request.RunID = "another-run"
	f.request.KnownRuns = []managedgit.Run{first}
	_, err := f.manager.Prepare(context.Background(), f.request)
	requireCode(t, err, "git.branch_conflict")
}

func TestCommitAndPushIncludesUntrackedFilesAndPreservesAgentCommits(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	write(t, run.Path, "README.md", "agent change\n")
	git(t, run.Path, "commit", "-am", "agent commit")
	agentSHA := git(t, run.Path, "rev-parse", "HEAD")
	write(t, run.Path, "new.txt", "new non-ignored file\n")
	write(t, run.Path, "secret.ignored", "ignored\n")
	result, err := f.manager.CommitAndPush(context.Background(), run, managedgit.Phase{Title: "Workspace and Git operations", RequireChanges: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Committed || result.SHA != git(t, f.remote, "rev-parse", "refs/heads/mergeyard/issue-7") {
		t.Fatalf("commit not pushed: %+v", result)
	}
	if got := git(t, f.remote, "show", "mergeyard/issue-7:new.txt"); got != "new non-ignored file" {
		t.Fatalf("untracked file not committed: %q", got)
	}
	if got := git(t, run.Path, "log", "-1", "--format=%s"); got != "mergeyard: #7 Workspace and Git operations" {
		t.Fatalf("wrong commit message: %q", got)
	}
	if git(t, run.Path, "rev-parse", "HEAD^") != agentSHA {
		t.Fatal("agent commit was not preserved")
	}
	if git(t, run.Path, "ls-files", "*.ignored") != "" {
		t.Fatal("ignored file was committed")
	}
}

func TestCommitAndPushRequiresDiffFromBase(t *testing.T) {
	for _, emptyCommit := range []bool{false, true} {
		t.Run(map[bool]string{false: "untouched", true: "empty-agent-commit"}[emptyCommit], func(t *testing.T) {
			f := newFixture(t)
			run := prepare(t, f)
			if emptyCommit {
				git(t, run.Path, "commit", "--allow-empty", "-m", "empty agent commit")
			}
			_, err := f.manager.CommitAndPush(context.Background(), run, managedgit.Phase{Title: "implement", RequireChanges: true})
			requireCode(t, err, "git.no_changes")
			if got := git(t, f.remote, "branch", "--list", "mergeyard/issue-7"); got != "" {
				t.Fatalf("empty implementation was pushed: %q", got)
			}
		})
	}
}

func TestCommitAndPushAllowsFixWithoutNewChanges(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	write(t, run.Path, "new.txt", "implementation\n")
	first, err := f.manager.CommitAndPush(context.Background(), run, managedgit.Phase{Title: "implement", RequireChanges: true})
	if err != nil {
		t.Fatal(err)
	}
	fix, err := f.manager.CommitAndPush(context.Background(), run, managedgit.Phase{Title: "fix", RequireChanges: false})
	if err != nil || fix.Committed || fix.SHA != first.SHA {
		t.Fatalf("unchanged fix should keep the existing commit: %+v, %v", fix, err)
	}
}

func TestCommitAndPushRejectsDivergenceEvenWithUnsafePushConfig(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	write(t, run.Path, "new.txt", "implementation\n")
	if _, err := f.manager.CommitAndPush(context.Background(), run, managedgit.Phase{Title: "implement", RequireChanges: true}); err != nil {
		t.Fatal(err)
	}
	git(t, f.seed, "fetch", "origin")
	git(t, f.seed, "checkout", "-b", run.Branch, "origin/"+run.Branch)
	write(t, f.seed, "README.md", "collaborator's changes\n")
	git(t, f.seed, "commit", "-am", "collaborator commit")
	git(t, f.seed, "push", "origin", run.Branch)
	remoteSHA := git(t, f.seed, "rev-parse", "HEAD")
	git(t, run.Path, "config", "remote.origin.mirror", "true")
	git(t, run.Path, "config", "remote.origin.push", "+refs/heads/*:refs/heads/*")
	write(t, run.Path, "new.txt", "local fix\n")
	result, err := f.manager.CommitAndPush(context.Background(), run, managedgit.Phase{Title: "fix"})
	requireCode(t, err, "git.push_rejected")
	if git(t, f.remote, "rev-parse", "refs/heads/"+run.Branch) != remoteSHA {
		t.Fatal("rejected push overwrote collaborator work")
	}
	if !result.Committed || git(t, run.Path, "show", "HEAD:new.txt") != "local fix" {
		t.Fatal("rejected push discarded local fix")
	}
}

func TestCleanupRemovesLocalArtifactsAndKeepsRemoteBranch(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	write(t, run.Path, "new.txt", "implementation\n")
	if _, err := f.manager.CommitAndPush(context.Background(), run, managedgit.Phase{Title: "implement", RequireChanges: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.Cleanup(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(run.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree not removed: %v", err)
	}
	if git(t, run.BasePath, "branch", "--list", run.Branch) != "" || strings.Contains(git(t, run.BasePath, "worktree", "list", "--porcelain"), run.ID) {
		t.Fatal("local branch or worktree metadata remains")
	}
	if git(t, f.remote, "branch", "--list", run.Branch) == "" {
		t.Fatal("cleanup deleted the remote branch")
	}
}

func TestPrepareUsesMachineURLRewriteForDefaultGitHubRemote(t *testing.T) {
	f := newFixture(t)
	configPath := filepath.Join(t.TempDir(), "gitconfig")
	git(t, f.seed, "config", "--file", configPath, "url."+f.remote+".insteadOf", "https://github.com/owner/repo.git")
	t.Setenv("GIT_CONFIG_GLOBAL", configPath)
	f.request.RemoteURL = ""
	run := prepare(t, f)
	write(t, run.Path, "new.txt", "using existing Git transport config\n")
	if _, err := f.manager.CommitAndPush(context.Background(), run, managedgit.Phase{Title: "implement", RequireChanges: true}); err != nil {
		t.Fatal(err)
	}
	if git(t, f.remote, "show", "mergeyard/issue-7:new.txt") != "using existing Git transport config" {
		t.Fatal("default GitHub remote did not use machine transport configuration")
	}
}

func TestPrepareRecoversOwnedWorktreeWithMissingDirectory(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	if err := os.RemoveAll(run.Path); err != nil {
		t.Fatal(err)
	}
	f.request.KnownRuns = []managedgit.Run{run}
	resumed := prepare(t, f)
	if git(t, resumed.Path, "rev-parse", "HEAD") != run.BaseSHA {
		t.Fatal("recovered worktree is not on the original base")
	}
}

// interruptWorktreeCreation prepares the shared base with a setup run, then
// makes the requested run's worktree creation hit its deadline inside a
// post-checkout hook that stalls only in run worktrees. It returns the partial
// worktree path, with the branch and intent file left behind and the hook gone.
func interruptWorktreeCreation(t *testing.T, f fixture) string {
	t.Helper()
	setup := f.request
	setup.RunID, setup.IssueNumber = "setup", 6
	if _, err := f.manager.Prepare(context.Background(), setup); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(f.root, "repos", "owner-repo", "base", ".git", "hooks", "post-checkout")
	script := `#!/bin/sh
case "$PWD" in
  */worktrees/*) /bin/sleep 30 ;;
esac
exit 0
`
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
	path, err := managedgit.WorktreePath(f.root, f.request.Repository, f.request.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("interrupted attempt did not leave a worktree directory: %v", err)
	}
	base := filepath.Join(f.root, "repos", "owner-repo", "base")
	if err := exec.Command("git", "-C", base, "show-ref", "--verify", "--quiet", "refs/heads/mergeyard/issue-7").Run(); err != nil {
		t.Fatalf("interrupted attempt did not leave the branch: %v", err)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPrepareRecoversWorktreeCreationInterruptedByDeadline(t *testing.T) {
	f := newFixture(t)
	interruptWorktreeCreation(t, f)
	// The next attempt has no saved ownership, only the recorded intent.
	run := prepare(t, f)
	if run.Path != filepath.Join(f.root, "worktrees", "owner-repo", "run-1") {
		t.Fatalf("recovery created the wrong worktree: %+v", run)
	}
	if got := git(t, run.Path, "branch", "--show-current"); got != run.Branch {
		t.Fatalf("recovered worktree branch = %q", got)
	}
	if got := git(t, run.Path, "rev-parse", "HEAD"); got != run.BaseSHA {
		t.Fatalf("recovered worktree started at %s, want %s", got, run.BaseSHA)
	}
	if got := git(t, run.Path, "status", "--porcelain"); got != "" {
		t.Fatalf("recovered worktree is not clean: %q", got)
	}
}

func TestPreparePreservesInterruptedWorktreeWithCommits(t *testing.T) {
	f := newFixture(t)
	path := interruptWorktreeCreation(t, f)
	// Work in the interrupted tree moves the branch past its recorded start.
	write(t, path, "manual.txt", "human work\n")
	git(t, path, "add", "manual.txt")
	git(t, path, "commit", "-m", "manual commit")
	head := git(t, path, "rev-parse", "HEAD")
	_, err := f.manager.Prepare(context.Background(), f.request)
	requireCode(t, err, "git.branch_conflict")
	if got := git(t, path, "rev-parse", "HEAD"); got != head {
		t.Fatal("interrupted worktree with commits was changed")
	}
}

func TestPreparePreservesInterruptedWorktreeSwitchedToAnotherBranch(t *testing.T) {
	f := newFixture(t)
	path := interruptWorktreeCreation(t, f)
	git(t, path, "checkout", "-b", "human-work")
	write(t, path, "human.txt", "uncommitted human work\n")
	_, err := f.manager.Prepare(context.Background(), f.request)
	requireCode(t, err, "git.branch_conflict")
	if data, err := os.ReadFile(filepath.Join(path, "human.txt")); err != nil || string(data) != "uncommitted human work\n" {
		t.Fatalf("foreign worktree was changed: %q, %v", data, err)
	}
	if got := git(t, path, "branch", "--show-current"); got != "human-work" {
		t.Fatalf("foreign worktree branch = %q", got)
	}
}

func TestPreparePreservesInterruptedWorktreeWithRemovedHead(t *testing.T) {
	f := newFixture(t)
	path := interruptWorktreeCreation(t, f)
	git(t, path, "checkout", "-b", "human-work")
	write(t, path, "human.txt", "uncommitted human work\n")
	base := filepath.Join(f.root, "repos", "owner-repo", "base")
	if err := os.Remove(filepath.Join(base, ".git", "worktrees", f.request.RunID, "HEAD")); err != nil {
		t.Fatal(err)
	}
	_, err := f.manager.Prepare(context.Background(), f.request)
	requireCode(t, err, "git.branch_conflict")
	if data, err := os.ReadFile(filepath.Join(path, "human.txt")); err != nil || string(data) != "uncommitted human work\n" {
		t.Fatalf("worktree with removed HEAD was changed: %q, %v", data, err)
	}
}

func TestPreparePreservesInterruptedWorktreeWithMissingMetadata(t *testing.T) {
	f := newFixture(t)
	path := interruptWorktreeCreation(t, f)
	write(t, path, "human.txt", "human work\n")
	base := filepath.Join(f.root, "repos", "owner-repo", "base")
	if err := os.RemoveAll(filepath.Join(base, ".git", "worktrees", f.request.RunID)); err != nil {
		t.Fatal(err)
	}
	_, err := f.manager.Prepare(context.Background(), f.request)
	requireCode(t, err, "git.branch_conflict")
	if data, err := os.ReadFile(filepath.Join(path, "human.txt")); err != nil || string(data) != "human work\n" {
		t.Fatalf("worktree with missing metadata was changed: %q, %v", data, err)
	}
}

// requireConflictNaming fails unless err is a branch conflict that names the
// preserved leftover, so a person can find the work Prepare refused to delete.
func requireConflictNaming(t *testing.T, err error, path string) {
	t.Helper()
	requireCode(t, err, "git.branch_conflict")
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("the conflict does not name the worktree %s: %v", path, err)
	}
}

func TestPreparePreservesInterruptedWorktreeWithAnUntrackedFile(t *testing.T) {
	f := newFixture(t)
	path := interruptWorktreeCreation(t, f)
	write(t, path, "human.txt", "uncommitted human work\n")
	_, err := f.manager.Prepare(context.Background(), f.request)
	requireConflictNaming(t, err, path)
	if data, err := os.ReadFile(filepath.Join(path, "human.txt")); err != nil || string(data) != "uncommitted human work\n" {
		t.Fatalf("the untracked file was deleted: %q, %v", data, err)
	}
}

func TestPreparePreservesInterruptedWorktreeWithAnEditedTrackedFile(t *testing.T) {
	f := newFixture(t)
	path := interruptWorktreeCreation(t, f)
	write(t, path, "README.md", "edited by a person\n")
	_, err := f.manager.Prepare(context.Background(), f.request)
	requireConflictNaming(t, err, path)
	if data, err := os.ReadFile(filepath.Join(path, "README.md")); err != nil || string(data) != "edited by a person\n" {
		t.Fatalf("the edit was deleted: %q, %v", data, err)
	}
}

func TestPreparePreservesAnUnfinishedInitializationHoldingAnExtraFile(t *testing.T) {
	f := newFixture(t)
	path := interruptBeforeHead(t, f)
	write(t, path, "human.txt", "uncommitted human work\n")
	_, err := f.manager.Prepare(context.Background(), f.request)
	requireConflictNaming(t, err, path)
	if data, err := os.ReadFile(filepath.Join(path, "human.txt")); err != nil || string(data) != "uncommitted human work\n" {
		t.Fatalf("the extra file was deleted: %q, %v", data, err)
	}
}

// interruptDuringCheckout makes the requested run's checkout stall in a smudge
// filter, so Git leaves a written HEAD, some of the files, and the
// initialization lock. It returns the base path with the filter still set.
func interruptDuringCheckout(t *testing.T, f fixture) string {
	t.Helper()
	setup := f.request
	setup.RunID, setup.IssueNumber = "setup", 6
	if _, err := f.manager.Prepare(context.Background(), setup); err != nil {
		t.Fatal(err)
	}
	write(t, f.seed, ".gitattributes", "payload filter=slow\n")
	write(t, f.seed, "payload", "smudged content\n")
	git(t, f.seed, "add", ".")
	git(t, f.seed, "commit", "-m", "filtered payload")
	git(t, f.seed, "push", "origin", "main")

	base := filepath.Join(f.root, "repos", "owner-repo", "base")
	marker := filepath.Join(t.TempDir(), "smudge-started")
	smudge := filepath.Join(t.TempDir(), "smudge.sh")
	script := fmt.Sprintf("#!/bin/sh\ncase \"$PWD\" in\n  */worktrees/*) : > '%s'; /bin/sleep 30 ;;\nesac\ncat\n", marker)
	if err := os.WriteFile(smudge, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	git(t, base, "config", "filter.slow.smudge", smudge)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := f.manager.Prepare(ctx, f.request)
	requireCode(t, err, "git.timeout")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("smudge filter did not stall in the worktree: %v", err)
	}
	locks, err := filepath.Glob(filepath.Join(base, ".git", "worktrees", "*", "locked"))
	if err != nil || len(locks) == 0 {
		t.Fatalf("interrupted checkout did not leave an initialization lock: %v, %v", locks, err)
	}
	return base
}

func TestPrepareRecoversWorktreeInterruptedDuringCheckout(t *testing.T) {
	f := newFixture(t)
	base := interruptDuringCheckout(t, f)
	git(t, base, "config", "filter.slow.smudge", "cat")
	run := prepare(t, f)
	if data, err := os.ReadFile(filepath.Join(run.Path, "payload")); err != nil || string(data) != "smudged content\n" {
		t.Fatalf("recovered payload = %q, %v", data, err)
	}
}

func TestPreparePreservesAnInterruptedCheckoutHoldingAnExtraFile(t *testing.T) {
	f := newFixture(t)
	base := interruptDuringCheckout(t, f)
	git(t, base, "config", "filter.slow.smudge", "cat")
	path := filepath.Join(f.root, "worktrees", "owner-repo", f.request.RunID)
	write(t, path, "human.txt", "uncommitted human work\n")
	_, err := f.manager.Prepare(context.Background(), f.request)
	requireConflictNaming(t, err, path)
	if data, err := os.ReadFile(filepath.Join(path, "human.txt")); err != nil || string(data) != "uncommitted human work\n" {
		t.Fatalf("the extra file was deleted: %q, %v", data, err)
	}
}

func TestPrepareRecoversAnInterruptedCheckoutWithACutShortFile(t *testing.T) {
	f := newFixture(t)
	base := interruptDuringCheckout(t, f)
	git(t, base, "config", "filter.slow.smudge", "cat")
	path := filepath.Join(f.root, "worktrees", "owner-repo", f.request.RunID)
	// Git was writing this file when the deadline killed it.
	write(t, path, "payload", "smudg")
	run := prepare(t, f)
	if data, err := os.ReadFile(filepath.Join(run.Path, "payload")); err != nil || string(data) != "smudged content\n" {
		t.Fatalf("recovered payload = %q, %v", data, err)
	}
}

func TestPrepareRecoversAnUnfinishedInitializationWithACutShortConvertedFile(t *testing.T) {
	f := newFixture(t)
	write(t, f.seed, ".gitattributes", "*.txt eol=crlf\n")
	write(t, f.seed, "lines.txt", "one\ntwo\nthree\n")
	git(t, f.seed, "add", ".")
	git(t, f.seed, "commit", "-m", "converted file")
	git(t, f.seed, "push", "origin", "main")
	path := interruptBeforeHead(t, f)
	// Git writes the converted contents, so the cut-short file is a start of
	// those and not of the stored file.
	write(t, path, "lines.txt", "one\r\ntw")
	run := prepare(t, f)
	if data, err := os.ReadFile(filepath.Join(run.Path, "lines.txt")); err != nil || string(data) != "one\r\ntwo\r\nthree\r\n" {
		t.Fatalf("recovered file = %q, %v", data, err)
	}
}

func TestPrepareRecoversAnUnfinishedInitializationWithAnEmptyFile(t *testing.T) {
	f := newFixture(t)
	path := interruptBeforeHead(t, f)
	write(t, path, "README.md", "")
	run := prepare(t, f)
	if data, err := os.ReadFile(filepath.Join(run.Path, "README.md")); err != nil || string(data) != "initial\n" {
		t.Fatalf("recovered README = %q, %v", data, err)
	}
}

func TestPreparePreservesAnUnfinishedInitializationWithAnEditedFile(t *testing.T) {
	f := newFixture(t)
	path := interruptBeforeHead(t, f)
	write(t, path, "README.md", "edited by a person\n")
	_, err := f.manager.Prepare(context.Background(), f.request)
	requireConflictNaming(t, err, path)
	if data, err := os.ReadFile(filepath.Join(path, "README.md")); err != nil || string(data) != "edited by a person\n" {
		t.Fatalf("the edit was deleted: %q, %v", data, err)
	}
}

func TestPrepareRecoversAnInterruptedCreationWhoseWorktreeIsGone(t *testing.T) {
	f := newFixture(t)
	path := interruptWorktreeCreation(t, f)
	// Only the branch and the registration remain.
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	run := prepare(t, f)
	if got := git(t, run.Path, "branch", "--show-current"); got != run.Branch {
		t.Fatalf("recovered worktree branch = %q", got)
	}
}

func TestPrepareRecoversAnInterruptedCreationLeftEmpty(t *testing.T) {
	f := newFixture(t)
	path := interruptWorktreeCreation(t, f)
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	run := prepare(t, f)
	if got := git(t, run.Path, "branch", "--show-current"); got != run.Branch {
		t.Fatalf("recovered worktree branch = %q", got)
	}
}

func TestPrepareRecoversWorktreeInterruptedBeforeHead(t *testing.T) {
	f := newFixture(t)
	setup := f.request
	setup.RunID, setup.IssueNumber = "setup", 6
	if _, err := f.manager.Prepare(context.Background(), setup); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(f.root, "repos", "owner-repo", "base")
	marker := filepath.Join(t.TempDir(), "head-prepared")
	hook := filepath.Join(base, ".git", "hooks", "reference-transaction")
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
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("reference-transaction hook did not stall before HEAD: %v", err)
	}
	locks, err := filepath.Glob(filepath.Join(base, ".git", "worktrees", "*", "locked"))
	if err != nil || len(locks) == 0 {
		t.Fatalf("interrupted initialization did not leave a lock: %v, %v", locks, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(locks[0]), "HEAD")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted initialization already wrote HEAD: %v", err)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	run := prepare(t, f)
	if got := git(t, run.Path, "branch", "--show-current"); got != run.Branch {
		t.Fatalf("recovered worktree branch = %q", got)
	}
}

// prepareIntentPath returns the file where preparation records that this run
// created its branch and worktree.
func prepareIntentPath(t *testing.T, f fixture) string {
	t.Helper()
	path, err := managedgit.PrepareIntentPath(f.root, f.request.Repository, f.request.RunID)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPrepareKeepsIntentUntilOwnershipIsConfirmed(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	intent := prepareIntentPath(t, f)
	if _, err := os.Stat(intent); err != nil {
		t.Fatalf("preparation intent did not survive creation: %v", err)
	}
	if err := f.manager.ConfirmOwnership(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(intent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("intent survived the saved ownership: %v", err)
	}
}

func TestPrepareKeepsIntentWhenOwnershipConfirmationIsCancelled(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	intent := prepareIntentPath(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.manager.ConfirmOwnership(ctx, run); err == nil {
		t.Fatal("a cancelled confirmation released the intent")
	}
	if _, err := os.Stat(intent); err != nil {
		t.Fatalf("the intent did not survive the cancelled confirmation: %v", err)
	}
	// The next attempt recovers the run's own worktree.
	resumed := prepare(t, f)
	if resumed.Path != run.Path || resumed.BaseSHA != run.BaseSHA {
		t.Fatalf("recovery changed the prepared run: %+v", resumed)
	}
}

func TestPrepareAdoptsCompletedWorktreeAfterInterruptedOwnershipSave(t *testing.T) {
	f := newFixture(t)
	first := prepare(t, f)
	before, err := os.Stat(first.Path)
	if err != nil {
		t.Fatal(err)
	}
	// The base advances while the ownership save is still missing.
	write(t, f.seed, "README.md", "new base\n")
	git(t, f.seed, "commit", "-am", "new base")
	git(t, f.seed, "push", "origin", "main")
	// The next attempt has no saved ownership, only the recorded intent.
	second := prepare(t, f)
	if second.BaseSHA != first.BaseSHA {
		t.Fatalf("adoption moved the comparison point: %+v", second)
	}
	if got := git(t, second.Path, "rev-parse", "HEAD"); got != first.BaseSHA {
		t.Fatalf("adopted worktree is at %s, want %s", got, first.BaseSHA)
	}
	after, err := os.Stat(second.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("the completed worktree was discarded instead of adopted")
	}
}

func TestPreparePreservesCompletedWorktreeThatChangedAfterCreation(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	// Work arrives in the worktree before the ownership is saved.
	write(t, run.Path, "human.txt", "uncommitted human work\n")
	_, err := f.manager.Prepare(context.Background(), f.request)
	requireCode(t, err, "git.branch_conflict")
	if data, err := os.ReadFile(filepath.Join(run.Path, "human.txt")); err != nil || string(data) != "uncommitted human work\n" {
		t.Fatalf("the completed worktree was changed: %q, %v", data, err)
	}
}

func TestPrepareDropsStaleIntentWhenOwnershipIsKnown(t *testing.T) {
	f := newFixture(t)
	first := prepare(t, f)
	intent := prepareIntentPath(t, f)
	f.request.KnownRuns = []managedgit.Run{first}
	prepare(t, f)
	if _, err := os.Stat(intent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale intent survived a known run: %v", err)
	}
}

func TestPrepareRefusesForeignWorktreeWithoutIntent(t *testing.T) {
	f := newFixture(t)
	path, err := managedgit.WorktreePath(f.root, f.request.Repository, f.request.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, path, "human.txt", "someone else's work\n")
	_, err = f.manager.Prepare(context.Background(), f.request)
	requireCode(t, err, "git.branch_conflict")
	if data, err := os.ReadFile(filepath.Join(path, "human.txt")); err != nil || string(data) != "someone else's work\n" {
		t.Fatalf("foreign worktree was changed: %q, %v", data, err)
	}
}

func TestPhaseStagesTrackedEditsAndDeletionsWithoutPushingOtherRefs(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	git(t, run.Path, "config", "push.followTags", "true")
	git(t, run.Path, "tag", "-a", "unrelated-tag", "-m", "keep local")
	git(t, run.Path, "config", "remote.origin.pushurl", filepath.Join(t.TempDir(), "wrong-remote.git"))
	write(t, run.Path, "README.md", "tracked edit\n")
	if err := os.Remove(filepath.Join(run.Path, ".gitignore")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.CommitAndPush(context.Background(), run, managedgit.Phase{Title: "edit and delete", RequireChanges: true}); err != nil {
		t.Fatal(err)
	}
	if git(t, f.remote, "show", "mergeyard/issue-7:README.md") != "tracked edit" {
		t.Fatal("tracked edit was not pushed")
	}
	if git(t, f.remote, "ls-tree", "--name-only", "mergeyard/issue-7", ".gitignore") != "" {
		t.Fatal("tracked deletion was not pushed")
	}
	if git(t, f.remote, "tag", "--list") != "" || git(t, f.remote, "rev-parse", "main") != run.BaseSHA {
		t.Fatal("phase push changed unrelated refs")
	}
}

func TestCleanupIsRepeatableAndRefusesDirtyWorktrees(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	write(t, run.Path, "partial.txt", "preserve\n")
	requireCode(t, f.manager.Cleanup(context.Background(), run), "git.cleanup")
	if data, err := os.ReadFile(filepath.Join(run.Path, "partial.txt")); err != nil || string(data) != "preserve\n" {
		t.Fatalf("cleanup discarded dirty files: %q, %v", data, err)
	}
	if err := os.Remove(filepath.Join(run.Path, "partial.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, run.Path, "build.ignored", "ignored build output\n")
	for range 2 {
		if err := f.manager.Cleanup(context.Background(), run); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCleanupWorktreeFinishesInterruptedRemovalWithIntent(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	// Simulate Git deleting the link before the rest of the tree when a
	// removal is killed partway.
	if err := os.Remove(filepath.Join(run.Path, ".git")); err != nil {
		t.Fatal(err)
	}
	write(t, run.Path, "leftover.txt", "partially removed\n")
	run.RemovalIntent = true
	if err := f.manager.CleanupWorktree(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(run.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted removal not finished: %v", err)
	}
	if strings.Contains(git(t, run.BasePath, "worktree", "list", "--porcelain"), run.Path) {
		t.Fatal("worktree registration remains")
	}
	if git(t, run.BasePath, "branch", "--list", run.Branch) == "" {
		t.Fatal("worktree cleanup deleted the local branch")
	}
}

func TestCleanupWorktreeFinishesInterruptedRemovalWithLink(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	control := filepath.Join(t.TempDir(), "interrupt")
	if err := os.WriteFile(control, nil, 0600); err != nil {
		t.Fatal(err)
	}
	// The first removal deletes tracked work but leaves the link, as a killed
	// real removal may, and fails. Later removals go straight to real Git.
	installGitWrapper(t, "worktree remove", `if [ -f "`+control+`" ]; then
  rm -f "`+control+`"
  for last; do :; done
  rm -f "$last/README.md"
  exit 1
fi`)
	requireCode(t, f.manager.CleanupWorktree(context.Background(), run), "git.cleanup")
	if _, err := os.Stat(filepath.Join(run.Path, ".git")); err != nil {
		t.Fatalf("fixture removed the worktree link: %v", err)
	}
	if _, err := os.Stat(filepath.Join(run.Path, "README.md")); !os.IsNotExist(err) {
		t.Fatal("fixture did not delete tracked work")
	}
	if err := f.manager.CleanupWorktree(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(run.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted removal with a surviving link not finished: %v", err)
	}
	if strings.Contains(git(t, run.BasePath, "worktree", "list", "--porcelain"), run.Path) {
		t.Fatal("worktree registration remains")
	}
}

func TestCleanupWorktreePreservesMissingLinkWithoutIntent(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	if err := os.Remove(filepath.Join(run.Path, ".git")); err != nil {
		t.Fatal(err)
	}
	write(t, run.Path, "human.txt", "preserve me\n")
	requireCode(t, f.manager.CleanupWorktree(context.Background(), run), "git.worktree_mismatch")
	if data, err := os.ReadFile(filepath.Join(run.Path, "human.txt")); err != nil || string(data) != "preserve me\n" {
		t.Fatalf("unexplained missing link discarded work: %q, %v", data, err)
	}
}

func TestCleanupWorktreeWithIntentStillPreservesDirtyWork(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	write(t, run.Path, "human.txt", "preserve me\n")
	run.RemovalIntent = true
	requireCode(t, f.manager.CleanupWorktree(context.Background(), run), "git.cleanup")
	if data, err := os.ReadFile(filepath.Join(run.Path, "human.txt")); err != nil || string(data) != "preserve me\n" {
		t.Fatalf("linked dirty work discarded despite intent: %q, %v", data, err)
	}
}

func TestCleanupWorktreeReportsLockedRegistrationAfterRecovery(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	if err := os.Remove(filepath.Join(run.Path, ".git")); err != nil {
		t.Fatal(err)
	}
	git(t, run.BasePath, "worktree", "lock", "--reason", "human inspection", run.Path)
	run.RemovalIntent = true
	requireCode(t, f.manager.CleanupWorktree(context.Background(), run), "git.cleanup")
	if !strings.Contains(git(t, run.BasePath, "worktree", "list", "--porcelain"), run.Path) {
		t.Fatal("fixture lost the locked registration")
	}
	git(t, run.BasePath, "worktree", "unlock", run.Path)
	if err := f.manager.CleanupWorktree(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(run.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unlocked registration was not cleaned")
	}
}

func TestCleanupWorktreePreservesEditsAfterLockedRefusal(t *testing.T) {
	for _, edit := range []struct {
		name  string
		apply func(*testing.T, managedgit.Run)
		check func(*testing.T, managedgit.Run)
	}{
		{
			"untracked",
			func(t *testing.T, run managedgit.Run) { write(t, run.Path, "human.txt", "preserve me\n") },
			func(t *testing.T, run managedgit.Run) {
				if data, err := os.ReadFile(filepath.Join(run.Path, "human.txt")); err != nil || string(data) != "preserve me\n" {
					t.Fatalf("locked refusal lost untracked work: %q, %v", data, err)
				}
			},
		},
		{
			"modified",
			func(t *testing.T, run managedgit.Run) { write(t, run.Path, "README.md", "human edit\n") },
			func(t *testing.T, run managedgit.Run) {
				if data, err := os.ReadFile(filepath.Join(run.Path, "README.md")); err != nil || string(data) != "human edit\n" {
					t.Fatalf("locked refusal lost edited work: %q, %v", data, err)
				}
			},
		},
	} {
		t.Run(edit.name, func(t *testing.T) {
			f := newFixture(t)
			run := prepare(t, f)
			git(t, run.BasePath, "worktree", "lock", "--reason", "human inspection", run.Path)
			requireCode(t, f.manager.CleanupWorktree(context.Background(), run), "git.cleanup")
			intent := filepath.Join(filepath.Dir(run.Path), run.ID+".cleanup")
			if _, err := os.Stat(intent); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refused removal kept its cleanup intent: %v", err)
			}
			if _, err := os.Stat(filepath.Join(run.Path, ".git")); err != nil {
				t.Fatalf("refused removal deleted the worktree: %v", err)
			}
			edit.apply(t, run)
			// A retry must preserve the human edit instead of trusting the
			// boundary from the refusal.
			requireCode(t, f.manager.CleanupWorktree(context.Background(), run), "git.cleanup")
			edit.check(t, run)
			if _, err := os.Stat(filepath.Join(run.Path, ".git")); err != nil {
				t.Fatalf("locked worktree was deleted: %v", err)
			}
			git(t, run.BasePath, "worktree", "unlock", run.Path)
			if edit.name == "untracked" {
				if err := os.Remove(filepath.Join(run.Path, "human.txt")); err != nil {
					t.Fatal(err)
				}
			} else {
				git(t, run.Path, "checkout", "--", "README.md")
			}
			if err := f.manager.CleanupWorktree(context.Background(), run); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(run.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("worktree not removed after unlock")
			}
		})
	}
}

// writeCleanupIntentFixture creates the on-disk boundary cleanup writes before
// Git starts deleting, so tests can simulate a crash between the refusal and
// the boundary's removal.
func writeCleanupIntentFixture(t *testing.T, run managedgit.Run) string {
	t.Helper()
	path := filepath.Join(filepath.Dir(run.Path), run.ID+".cleanup")
	data := fmt.Sprintf(`{"id":%q,"path":%q,"base_path":%q,"branch":%q}`, run.ID, run.Path, run.BasePath, run.Branch)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCleanupWorktreeRechecksRetainedIntentAfterRefusal(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	intent := writeCleanupIntentFixture(t, run)
	git(t, run.BasePath, "worktree", "lock", "--reason", "human inspection", run.Path)
	write(t, run.Path, "human.txt", "preserve me\n")
	requireCode(t, f.manager.CleanupWorktree(context.Background(), run), "git.cleanup")
	if data, err := os.ReadFile(filepath.Join(run.Path, "human.txt")); err != nil || string(data) != "preserve me\n" {
		t.Fatalf("retained intent bypassed the lock: %q, %v", data, err)
	}
	if _, err := os.Stat(intent); err != nil {
		t.Fatalf("retained intent was lost while the worktree is protected: %v", err)
	}
	git(t, run.BasePath, "worktree", "unlock", run.Path)
	requireCode(t, f.manager.CleanupWorktree(context.Background(), run), "git.cleanup")
	if data, err := os.ReadFile(filepath.Join(run.Path, "human.txt")); err != nil || string(data) != "preserve me\n" {
		t.Fatalf("retained intent bypassed human work after unlock: %q, %v", data, err)
	}
	if err := os.Remove(filepath.Join(run.Path, "human.txt")); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.CleanupWorktree(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(run.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("worktree not removed after human work cleared")
	}
	if _, err := os.Stat(intent); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cleanup intent retained after removal")
	}
}

func TestGitOperationsRespectCancellation(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.manager.Prepare(ctx, f.request)
	requireCode(t, err, "git.canceled")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation cause: %v", err)
	}
}

func TestPrepareRejectsUnsafeInputs(t *testing.T) {
	for _, change := range []struct {
		name  string
		apply func(*managedgit.PrepareRequest)
	}{
		{"repository", func(r *managedgit.PrepareRequest) { r.Repository = "../repo" }},
		{"run-id", func(r *managedgit.PrepareRequest) { r.RunID = "../escape" }},
		{"issue", func(r *managedgit.PrepareRequest) { r.IssueNumber = 0 }},
		{"remote-option", func(r *managedgit.PrepareRequest) { r.RemoteURL = "--upload-pack=bad" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			f := newFixture(t)
			change.apply(&f.request)
			_, err := f.manager.Prepare(context.Background(), f.request)
			requireCode(t, err, "git.invalid_input")
		})
	}
}

func TestPhaseAndCleanupRefuseWrongWorktreeBranch(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	git(t, run.Path, "checkout", "-b", "user-branch")
	write(t, run.Path, "partial.txt", "user work\n")
	_, err := f.manager.CommitAndPush(context.Background(), run, managedgit.Phase{Title: "implement", RequireChanges: true})
	requireCode(t, err, "git.worktree_mismatch")
	requireCode(t, f.manager.Cleanup(context.Background(), run), "git.worktree_mismatch")
	if git(t, run.Path, "status", "--porcelain") != "?? partial.txt" {
		t.Fatal("wrong branch was staged or changed")
	}
}

func TestPrepareRefusesBaseSymlinkToUnmanagedCheckout(t *testing.T) {
	f := newFixture(t)
	base := filepath.Join(f.root, "repos", "owner-repo", "base")
	if err := os.MkdirAll(filepath.Dir(base), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.seed, base); err != nil {
		t.Fatal(err)
	}
	_, err := f.manager.Prepare(context.Background(), f.request)
	requireCode(t, err, "git.base_checkout")
	if git(t, f.seed, "branch", "--show-current") != "main" {
		t.Fatal("unmanaged checkout was changed")
	}
}

func TestCleanupKeepsUnpublishedRefAfterWorktreeRemoval(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	write(t, run.Path, "new.txt", "published\n")
	result, err := f.manager.CommitAndPush(context.Background(), run, managedgit.Phase{Title: "implement", RequireChanges: true})
	if err != nil {
		t.Fatal(err)
	}
	run.PublishedSHA = result.SHA
	if err := f.manager.CleanupWorktree(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	tree := git(t, run.BasePath, "rev-parse", result.SHA+"^{tree}")
	unpublished := git(t, run.BasePath, "commit-tree", tree, "-p", result.SHA, "-m", "unpublished work")
	git(t, run.BasePath, "update-ref", "refs/heads/"+run.Branch, unpublished)
	requireCode(t, f.manager.CleanupBranch(context.Background(), run), "git.unpublished_work")
	if git(t, run.BasePath, "rev-parse", "refs/heads/"+run.Branch) != unpublished {
		t.Fatal("lost unpublished ref after partial cleanup")
	}
}
func TestCleanupUsesMergedPublicationReceiptWhenRemoteBranchIsGone(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	write(t, run.Path, "new.txt", "published\n")
	result, err := f.manager.CommitAndPush(context.Background(), run, managedgit.Phase{Title: "implement", RequireChanges: true})
	if err != nil {
		t.Fatal(err)
	}
	run.PublishedSHA = result.SHA
	git(t, f.remote, "update-ref", "-d", "refs/heads/"+run.Branch)
	for range 2 {
		if err := f.manager.Cleanup(context.Background(), run); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(run.Path); !os.IsNotExist(err) {
		t.Fatal("published worktree not cleaned")
	}
	if git(t, run.BasePath, "branch", "--list", run.Branch) != "" {
		t.Fatal("published local branch not cleaned")
	}
}
