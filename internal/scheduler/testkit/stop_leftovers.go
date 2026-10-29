package testkit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// preparingRun creates a run that claimed an issue and started preparing,
// without the Git ownership a completed preparation saves.
func PreparingRun(t *testing.T, rt *app.Runtime, id string) workflow.Run {
	t.Helper()
	ctx := context.Background()
	if _, err := rt.Workflow.Transition(ctx, id, workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7}); err != nil {
		t.Fatal(err)
	}
	run, err := rt.Workflow.Transition(ctx, id, workflow.Request{Trigger: workflow.ClaimSucceeded})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// waitForRun waits until any run reaches the state, ignoring the other runs
// a stopped attempt left behind.
func WaitForRun(t *testing.T, s *scheduler.Scheduler, state workflow.State, phase workflow.Phase) workflow.Run {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range runs {
			if run.State == state && run.Phase == phase {
				return run
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no run reached %s/%s: %+v", state, phase, runs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// seedRemoteSubmodule adds a submodule to the fixture's seed repository and
// publishes it, so the next prepared checkout records a gitlink. A non-empty
// ignore setting is written to .gitmodules, so status hides submodule changes.
func SeedRemoteSubmodule(t *testing.T, remote, ignore string) {
	t.Helper()
	root := filepath.Dir(remote)
	moduleRemote := filepath.Join(root, "module.git")
	moduleSeed := filepath.Join(root, "module-seed")
	GitCommand(t, root, "init", "--bare", "--initial-branch=main", moduleRemote)
	GitCommand(t, root, "init", "--initial-branch=main", moduleSeed)
	if err := os.WriteFile(filepath.Join(moduleSeed, "module.txt"), []byte("module\n"), 0600); err != nil {
		t.Fatal(err)
	}
	GitCommand(t, moduleSeed, "add", ".")
	GitCommand(t, moduleSeed, "commit", "-m", "module")
	GitCommand(t, moduleSeed, "remote", "add", "origin", moduleRemote)
	GitCommand(t, moduleSeed, "push", "origin", "main")
	seed := filepath.Join(root, "seed")
	GitCommand(t, seed, "-c", "protocol.file.allow=always", "submodule", "add", "--", moduleRemote, "module")
	if ignore != "" {
		GitCommand(t, seed, "config", "-f", ".gitmodules", "submodule.module.ignore", ignore)
		GitCommand(t, seed, "add", ".gitmodules")
	}
	GitCommand(t, seed, "commit", "-m", "add submodule")
	GitCommand(t, seed, "push", "origin", "main")
}

// interruptPreparingCheckout leaves a clean, unlocked checkout from an
// interrupted preparation: Git finishes the checkout and stalls in its
// post-checkout hook, after the initialization lock is gone.
func InterruptPreparingCheckout(t *testing.T, rt *app.Runtime, git *managedgit.Manager, run workflow.Run) string {
	t.Helper()
	ctx := context.Background()
	if _, err := git.Prepare(ctx, managedgit.PrepareRequest{Repository: "owner/repo", RunID: "setup", IssueNumber: 6}); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(rt.Workspace.Root, "repos", "owner-repo", "base", ".git", "hooks", "post-checkout")
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
	interrupted, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := git.Prepare(interrupted, managedgit.PrepareRequest{Repository: "owner/repo", RunID: run.ID, IssueNumber: 7}); err == nil {
		t.Fatal("the interrupted creation was not interrupted")
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	path, err := managedgit.WorktreePath(rt.Workspace.Root, "owner/repo", run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the interrupted creation left no worktree: %v", err)
	}
	return path
}

// unownedStates are the states a run without saved Git ownership can be in
// when a stop arrives: still PREPARING, NEEDS_ATTENTION after a failed Prepare,
// and CLAIMING once a Retry moved it on. In each, the preparation intent names
// the branch and the worktree an earlier attempt left.
var UnownedStates = []workflow.State{workflow.Preparing, workflow.NeedsAttention, workflow.Claiming}

// runInState moves a run that started preparing into the state a stop finds it
// in. It saves no Git ownership.
func RunInState(t *testing.T, rt *app.Runtime, id string, state workflow.State) workflow.Run {
	t.Helper()
	ctx := context.Background()
	run := PreparingRun(t, rt, id)
	if state == workflow.Preparing {
		return run
	}
	run, err := rt.Workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.OperationFailed, Failure: &fault.Error{Code: "git.prepare_failed", Message: "Could not prepare the worktree"}})
	if err != nil {
		t.Fatal(err)
	}
	if state == workflow.Claiming {
		run, err = rt.Workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.Retry, NextState: workflow.Claiming})
		if err != nil {
			t.Fatal(err)
		}
	}
	if run.State != state {
		t.Fatalf("the run is %s, not %s", run.State, state)
	}
	return run
}

// saveOwnership records the Git ownership a completed preparation saves.
func SaveOwnership(t *testing.T, rt *app.Runtime, run workflow.Run, prepared managedgit.Run) {
	t.Helper()
	gitJSON, err := json.Marshal(prepared)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.DB.Exec(`INSERT INTO scheduler_runs(run_id,issue_json,git_json) VALUES(?,?,?)`, run.ID, "{}", string(gitJSON)); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.DB.Exec(`UPDATE runs SET branch=?, worktree_path=? WHERE id=?`, prepared.Branch, prepared.Path, run.ID); err != nil {
		t.Fatal(err)
	}
}

func StopSavedOwnership(t *testing.T, state workflow.State) {
	t.Helper()
	s, rt, api, _, _, _ := TmuxFlow(t, SuccessfulScript)
	ctx := context.Background()
	run := RunInState(t, rt, "owned", state)
	prepared, err := TestGitManager(rt.Workspace).Prepare(ctx, managedgit.PrepareRequest{Repository: "owner/repo", RunID: run.ID, IssueNumber: 7})
	if err != nil {
		t.Fatal(err)
	}
	SaveOwnership(t, rt, run, prepared)
	if err := s.Stop(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(prepared.Path); err != nil {
		t.Fatalf("the owned worktree was discarded: %v", err)
	}
	base := filepath.Join(rt.Workspace.Root, "repos", "owner-repo", "base")
	if branch := GitCommand(t, base, "branch", "--list", "mergeyard/issue-7"); branch == "" {
		t.Fatal("the owned branch was deleted")
	}
	if !Has(api.Issues["owner/repo"][0], "agent-needs-attention") {
		t.Fatalf("the owned work did not flag the issue: %+v", api.Issues["owner/repo"][0].Labels)
	}
}

// requireStoppedUnverified proves that a stop finished on leftovers it could
// not check: the run is STOPPED, nothing was deleted, the issue is flagged, and
// the comment names the run's branch and worktree as unverified.
func RequireStoppedUnverified(t *testing.T, rt *app.Runtime, api *FakeGitHub, run workflow.Run, prepared managedgit.Run) {
	t.Helper()
	stopped, err := rt.Workflow.Get(context.Background(), run.ID)
	if err != nil || stopped.State != workflow.Stopped {
		t.Fatalf("stop: %+v %v", stopped, err)
	}
	if _, err := os.Stat(prepared.Path); err != nil {
		t.Fatalf("the unverified worktree was removed: %v", err)
	}
	if !Has(api.Issues["owner/repo"][0], "agent-needs-attention") {
		t.Fatalf("the unverified leftovers did not flag the issue: %+v", api.Issues["owner/repo"][0].Labels)
	}
	comment, ok := api.LeftoverComments[7]
	if want := filepath.ToSlash(filepath.Join("worktrees", "owner-repo", run.ID)); !ok || !comment.Unverified || comment.Branch != "mergeyard/issue-7" || comment.Worktree != want {
		t.Fatalf("the unverified leftovers were not named on the issue: %+v", api.LeftoverComments)
	}
}

// stoppedWithUnverifiedLeftovers starts a run whose preparation was
// interrupted, makes the first Stop keep the leftovers unchecked, and fails its
// label removal so the run stays pending. The returned gate keeps Git status
// failing until it is removed, so a retry can check the leftovers again.
func StoppedWithUnverifiedLeftovers(t *testing.T, s *scheduler.Scheduler, rt *app.Runtime, api *FakeGitHub) (workflow.Run, managedgit.Run, string) {
	t.Helper()
	ctx := context.Background()
	run := PreparingRun(t, rt, "interrupted")
	prepared, err := TestGitManager(rt.Workspace).Prepare(ctx, managedgit.PrepareRequest{Repository: "owner/repo", RunID: run.ID, IssueNumber: 7})
	if err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(t.TempDir(), "fail-git-status")
	InstallGitWrapper(t, "status", `if [ -e "`+gate+`" ]; then exit 1; fi`)
	if err := os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	// The run may already carry the flag from an earlier failure, so the
	// retry also has to withdraw it once the leftovers are gone.
	api.Issues["owner/repo"][0].Labels = append(api.Issues["owner/repo"][0].Labels, github.Label{Name: "agent-needs-attention"})
	FailLabelRemovals(api, 1)
	if err := s.Stop(ctx, run.ID); err == nil {
		t.Fatal("the failed label removal did not fail the stop")
	}
	RequireRecordedLeftovers(t, rt, run.ID, prepared)
	return run, prepared, gate
}

// failLabelRemovals makes the next count label removals fail with a GitHub
// outage.
func FailLabelRemovals(api *FakeGitHub, count int) {
	api.Mutate = func(action, _ string, _ int, _ string) error {
		if action == "remove" && count > 0 {
			count--
			return &fault.Error{Code: "github.unavailable", Message: "GitHub is unavailable"}
		}
		return nil
	}
}

// recordedLeftovers returns the branch and worktree the run records.
func RecordedLeftovers(t *testing.T, rt *app.Runtime, id string) (string, string) {
	t.Helper()
	var branch, path string
	if err := rt.DB.QueryRow("SELECT COALESCE(branch,''), COALESCE(worktree_path,'') FROM runs WHERE id=?", id).Scan(&branch, &path); err != nil {
		t.Fatal(err)
	}
	return branch, path
}

func RequireRecordedLeftovers(t *testing.T, rt *app.Runtime, id string, prepared managedgit.Run) {
	t.Helper()
	if branch, path := RecordedLeftovers(t, rt, id); branch != prepared.Branch || path != prepared.Path {
		t.Fatalf("the leftovers are not recorded on the run: branch %q, worktree %q", branch, path)
	}
}

func RequireNoRecordedLeftovers(t *testing.T, rt *app.Runtime, id string) {
	t.Helper()
	if branch, path := RecordedLeftovers(t, rt, id); branch != "" || path != "" {
		t.Fatalf("the run still records leftovers that are gone: branch %q, worktree %q", branch, path)
	}
}

func LeftoverEvents(t *testing.T, rt *app.Runtime, id string) int {
	t.Helper()
	var count int
	if err := rt.DB.QueryRow("SELECT count(*) FROM events WHERE run_id=? AND type='run.stop_leftovers'", id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
