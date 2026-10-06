package git_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
	if resumed != first {
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
