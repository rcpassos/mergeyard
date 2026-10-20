package scheduler_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
func preparingRun(t *testing.T, rt *app.Runtime, id string) workflow.Run {
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
func waitForRun(t *testing.T, s *scheduler.Scheduler, state workflow.State, phase workflow.Phase) workflow.Run {
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

// TestStopPreparingRunReleasesInterruptedSetupForTheNextRun proves that
// stopping a run whose worktree setup was interrupted releases the leftovers
// it owns, so the next run for the same issue prepares instead of refusing
// them.
func TestStopPreparingRunReleasesInterruptedSetupForTheNextRun(t *testing.T) {
	s, rt, api, _, _, _ := localFlow(t, successfulScript)
	ctx := context.Background()
	run := preparingRun(t, rt, "interrupted")
	// The preparation created the branch and the worktree, but the ownership
	// was never saved.
	prepared, err := managedgit.New(rt.Workspace).Prepare(ctx, managedgit.PrepareRequest{Repository: "owner/repo", RunID: run.ID, IssueNumber: 7})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	stopped, err := rt.Workflow.Get(ctx, run.ID)
	if err != nil || stopped.State != workflow.Stopped {
		t.Fatalf("stop: %+v %v", stopped, err)
	}
	if _, err := os.Stat(prepared.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the interrupted worktree survived the stop: %v", err)
	}
	base := filepath.Join(rt.Workspace.Root, "repos", "owner-repo", "base")
	if branch := gitCommand(t, base, "branch", "--list", "mergeyard/issue-7"); branch != "" {
		t.Fatalf("the interrupted branch survived the stop: %s", branch)
	}
	issue := api.issues["owner/repo"][0]
	if has(issue, "agent-needs-attention") {
		t.Fatalf("the released leftovers flagged the issue: %+v", issue.Labels)
	}
	// The issue is queued again, and the next run prepares it.
	api.issues["owner/repo"][0].Labels = []github.Label{{Name: "ready-for-agent"}}
	next := waitForRun(t, s, workflow.Active, workflow.Review)
	if next.ID == run.ID {
		t.Fatalf("the stopped run was resumed: %+v", next)
	}
	if next.LastErrorCode == "git.branch_conflict" {
		t.Fatalf("the next run refused the released branch: %+v", next)
	}
}

// TestStopPreparingRunReleasesAnInterruptedCreation proves that stopping a run
// whose worktree creation was interrupted removes the partial worktree and the
// branch it left, so the next run for the same issue prepares.
func TestStopPreparingRunReleasesAnInterruptedCreation(t *testing.T) {
	s, rt, api, _, _, _ := localFlow(t, successfulScript)
	ctx := context.Background()
	run := preparingRun(t, rt, "interrupted")
	git := managedgit.New(rt.Workspace)
	// The managed base exists before the creation is interrupted.
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
	_, err := git.Prepare(interrupted, managedgit.PrepareRequest{Repository: "owner/repo", RunID: run.ID, IssueNumber: 7})
	if err == nil {
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
	if err := s.Stop(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the interrupted worktree survived the stop: %v", err)
	}
	base := filepath.Join(rt.Workspace.Root, "repos", "owner-repo", "base")
	if branch := gitCommand(t, base, "branch", "--list", "mergeyard/issue-7"); branch != "" {
		t.Fatalf("the interrupted branch survived the stop: %s", branch)
	}
	// The issue is queued again, and the next run prepares it.
	api.issues["owner/repo"][0].Labels = []github.Label{{Name: "ready-for-agent"}}
	next := waitForRun(t, s, workflow.Active, workflow.Review)
	if next.ID == run.ID {
		t.Fatalf("the stopped run was resumed: %+v", next)
	}
	if next.LastErrorCode == "git.branch_conflict" {
		t.Fatalf("the next run refused the released branch: %+v", next)
	}
}

// TestStopPreparingRunKeepsHumanWorkInAnUnfinishedInitialization proves that a
// directory Git never finished initializing is not removed while it holds
// files a person added: the stop keeps them, names them, and flags the issue.
func TestStopPreparingRunKeepsHumanWorkInAnUnfinishedInitialization(t *testing.T) {
	s, rt, api, _, _, _ := localFlow(t, successfulScript)
	ctx := context.Background()
	run := preparingRun(t, rt, "interrupted")
	git := managedgit.New(rt.Workspace)
	// The managed base exists before the creation is interrupted.
	if _, err := git.Prepare(ctx, managedgit.PrepareRequest{Repository: "owner/repo", RunID: "setup", IssueNumber: 6}); err != nil {
		t.Fatal(err)
	}
	// The reference transaction of HEAD stalls, so Git never finishes the
	// checkout it started.
	hook := filepath.Join(rt.Workspace.Root, "repos", "owner-repo", "base", ".git", "hooks", "reference-transaction")
	script := `#!/bin/sh
[ "$1" = prepared ] || exit 0
while read old new ref; do
  if [ "$ref" = HEAD ] && [ "$new" = ref:refs/heads/mergeyard/issue-7 ]; then
    /bin/sleep 30
  fi
done
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
	if err := os.WriteFile(filepath.Join(path, "human.txt"), []byte("uncommitted human work\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(path, "human.txt")); err != nil || string(data) != "uncommitted human work\n" {
		t.Fatalf("the human work was deleted: %q, %v", data, err)
	}
	issue := api.issues["owner/repo"][0]
	if !has(issue, "agent-needs-attention") {
		t.Fatalf("the preserved work did not flag the issue: %+v", issue.Labels)
	}
	comment, ok := api.leftoverComments[7]
	if !ok || comment.Path != path {
		t.Fatalf("the preserved work was not named on the issue: %+v", api.leftoverComments)
	}
}

// TestStopPreparingRunKeepsTheWorktreeOfASavedOwnership proves that a
// preparation whose ownership was saved keeps its worktree: that work belongs
// to the run, and Stop preserves it.
func TestStopPreparingRunKeepsTheWorktreeOfASavedOwnership(t *testing.T) {
	s, rt, api, _, _, _ := localFlow(t, successfulScript)
	ctx := context.Background()
	run := preparingRun(t, rt, "owned")
	prepared, err := managedgit.New(rt.Workspace).Prepare(ctx, managedgit.PrepareRequest{Repository: "owner/repo", RunID: run.ID, IssueNumber: 7})
	if err != nil {
		t.Fatal(err)
	}
	// The ownership was saved before the interruption.
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
	if err := s.Stop(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(prepared.Path); err != nil {
		t.Fatalf("the owned worktree was discarded: %v", err)
	}
	base := filepath.Join(rt.Workspace.Root, "repos", "owner-repo", "base")
	if branch := gitCommand(t, base, "branch", "--list", "mergeyard/issue-7"); branch == "" {
		t.Fatal("the owned branch was deleted")
	}
	issue := api.issues["owner/repo"][0]
	if !has(issue, "agent-needs-attention") {
		t.Fatalf("the owned work did not flag the issue: %+v", issue.Labels)
	}
}

// TestStopPreparingRunKeepsLeftoversThatHoldWork proves that leftovers with
// changes are never deleted: the stop names them on the issue and flags it
// for attention, and the next run is told exactly what blocks it.
func TestStopPreparingRunKeepsLeftoversThatHoldWork(t *testing.T) {
	s, rt, api, _, _, _ := localFlow(t, successfulScript)
	ctx := context.Background()
	run := preparingRun(t, rt, "interrupted")
	prepared, err := managedgit.New(rt.Workspace).Prepare(ctx, managedgit.PrepareRequest{Repository: "owner/repo", RunID: run.ID, IssueNumber: 7})
	if err != nil {
		t.Fatal(err)
	}
	// Work arrives in the worktree before the ownership is saved.
	if err := os.WriteFile(filepath.Join(prepared.Path, "human.txt"), []byte("work\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(prepared.Path, "human.txt")); err != nil || string(data) != "work\n" {
		t.Fatalf("the changed worktree was discarded: %q, %v", data, err)
	}
	issue := api.issues["owner/repo"][0]
	if !has(issue, "agent-needs-attention") {
		t.Fatalf("the preserved leftovers did not flag the issue: %+v", issue.Labels)
	}
	comment, ok := api.leftoverComments[7]
	if !ok {
		t.Fatalf("the preserved leftovers were not named on the issue: %+v", api.leftoverComments)
	}
	if comment.Branch != "mergeyard/issue-7" || comment.Path != prepared.Path {
		t.Fatalf("the explanation does not name the leftovers: %+v", comment)
	}
	// The next run for the same issue is told what blocks it.
	api.issues["owner/repo"][0].Labels = []github.Label{{Name: "ready-for-agent"}}
	blocked := waitForRun(t, s, workflow.NeedsAttention, "")
	if blocked.ID == run.ID {
		t.Fatalf("the stopped run was resumed: %+v", blocked)
	}
	if blocked.LastErrorCode != "git.branch_conflict" ||
		!strings.Contains(blocked.LastErrorMessage, "mergeyard/issue-7") ||
		!strings.Contains(blocked.LastErrorMessage, prepared.Path) {
		t.Fatalf("the next run did not name the blocker: %+v", blocked)
	}
}

// runAfterFailedPrepare moves a run that started preparing into the state a
// stop finds it in after the Prepare failed: NEEDS_ATTENTION, or CLAIMING once
// a Retry moved it on. It saves no Git ownership.
func runAfterFailedPrepare(t *testing.T, rt *app.Runtime, id string, state workflow.State) workflow.Run {
	t.Helper()
	ctx := context.Background()
	run := preparingRun(t, rt, id)
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

// TestStopReleasesLeftoversOfAnUnownedRunInAnyState proves that the release
// does not depend on the run still preparing: a Prepare that failed leaves the
// run in NEEDS_ATTENTION, and a Retry of it leaves the run in CLAIMING, both
// with the earlier attempt's branch and worktree still named by its intent.
func TestStopReleasesLeftoversOfAnUnownedRunInAnyState(t *testing.T) {
	for _, state := range []workflow.State{workflow.NeedsAttention, workflow.Claiming} {
		t.Run(string(state), func(t *testing.T) {
			s, rt, api, _, _, _ := localFlow(t, successfulScript)
			ctx := context.Background()
			run := runAfterFailedPrepare(t, rt, "interrupted", state)
			prepared, err := managedgit.New(rt.Workspace).Prepare(ctx, managedgit.PrepareRequest{Repository: "owner/repo", RunID: run.ID, IssueNumber: 7})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Stop(ctx, run.ID); err != nil {
				t.Fatal(err)
			}
			stopped, err := rt.Workflow.Get(ctx, run.ID)
			if err != nil || stopped.State != workflow.Stopped {
				t.Fatalf("stop: %+v %v", stopped, err)
			}
			if _, err := os.Stat(prepared.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the leftover worktree survived the stop: %v", err)
			}
			base := filepath.Join(rt.Workspace.Root, "repos", "owner-repo", "base")
			if branch := gitCommand(t, base, "branch", "--list", "mergeyard/issue-7"); branch != "" {
				t.Fatalf("the leftover branch survived the stop: %s", branch)
			}
			if has(api.issues["owner/repo"][0], "agent-needs-attention") {
				t.Fatalf("the released leftovers flagged the issue: %+v", api.issues["owner/repo"][0].Labels)
			}
			if _, ok := api.leftoverComments[7]; ok {
				t.Fatalf("released leftovers were explained on the issue: %+v", api.leftoverComments)
			}
			// The issue is queued again, and the next run prepares it.
			api.issues["owner/repo"][0].Labels = []github.Label{{Name: "ready-for-agent"}}
			next := waitForRun(t, s, workflow.Active, workflow.Review)
			if next.ID == run.ID {
				t.Fatalf("the stopped run was resumed: %+v", next)
			}
			if next.LastErrorCode == "git.branch_conflict" {
				t.Fatalf("the next run refused the released branch: %+v", next)
			}
		})
	}
}

// TestStopKeepsLeftoversThatHoldWorkOfAnUnownedRunInAnyState proves that work
// in the leftovers is preserved and named on the issue whatever the state of
// the run that stops.
func TestStopKeepsLeftoversThatHoldWorkOfAnUnownedRunInAnyState(t *testing.T) {
	for _, state := range []workflow.State{workflow.NeedsAttention, workflow.Claiming} {
		t.Run(string(state), func(t *testing.T) {
			s, rt, api, _, _, _ := localFlow(t, successfulScript)
			ctx := context.Background()
			run := runAfterFailedPrepare(t, rt, "interrupted", state)
			prepared, err := managedgit.New(rt.Workspace).Prepare(ctx, managedgit.PrepareRequest{Repository: "owner/repo", RunID: run.ID, IssueNumber: 7})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(prepared.Path, "human.txt"), []byte("work\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := s.Stop(ctx, run.ID); err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(filepath.Join(prepared.Path, "human.txt")); err != nil || string(data) != "work\n" {
				t.Fatalf("the changed worktree was discarded: %q, %v", data, err)
			}
			if !has(api.issues["owner/repo"][0], "agent-needs-attention") {
				t.Fatalf("the preserved leftovers did not flag the issue: %+v", api.issues["owner/repo"][0].Labels)
			}
			comment, ok := api.leftoverComments[7]
			if !ok {
				t.Fatalf("the preserved leftovers were not named on the issue: %+v", api.leftoverComments)
			}
			if comment.Branch != "mergeyard/issue-7" || comment.Path != prepared.Path {
				t.Fatalf("the explanation does not name the leftovers: %+v", comment)
			}
		})
	}
}

// TestStopKeepsTheWorktreeOfASavedOwnershipInAnyState proves that a run that
// saved its ownership keeps its branch and worktree on Stop, whatever state a
// failure or a Retry moved it to.
func TestStopKeepsTheWorktreeOfASavedOwnershipInAnyState(t *testing.T) {
	for _, state := range []workflow.State{workflow.NeedsAttention, workflow.Claiming} {
		t.Run(string(state), func(t *testing.T) {
			stopSavedOwnership(t, state)
		})
	}
}

func stopSavedOwnership(t *testing.T, state workflow.State) {
	t.Helper()
	s, rt, api, _, _, _ := localFlow(t, successfulScript)
	ctx := context.Background()
	run := runAfterFailedPrepare(t, rt, "owned", state)
	prepared, err := managedgit.New(rt.Workspace).Prepare(ctx, managedgit.PrepareRequest{Repository: "owner/repo", RunID: run.ID, IssueNumber: 7})
	if err != nil {
		t.Fatal(err)
	}
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
	if err := s.Stop(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(prepared.Path); err != nil {
		t.Fatalf("the owned worktree was discarded: %v", err)
	}
	base := filepath.Join(rt.Workspace.Root, "repos", "owner-repo", "base")
	if branch := gitCommand(t, base, "branch", "--list", "mergeyard/issue-7"); branch == "" {
		t.Fatal("the owned branch was deleted")
	}
	if !has(api.issues["owner/repo"][0], "agent-needs-attention") {
		t.Fatalf("the owned work did not flag the issue: %+v", api.issues["owner/repo"][0].Labels)
	}
}

// TestStopWithoutAnIntentChangesNothingInAnyState proves that a run whose
// preparation never created a branch or worktree has nothing to release: it stops without a flag or an
// explanation.
func TestStopWithoutAnIntentChangesNothingInAnyState(t *testing.T) {
	for _, state := range []workflow.State{workflow.NeedsAttention, workflow.Claiming} {
		t.Run(string(state), func(t *testing.T) {
			s, rt, api, _, _, _ := localFlow(t, successfulScript)
			ctx := context.Background()
			run := runAfterFailedPrepare(t, rt, "untouched", state)
			if err := s.Stop(ctx, run.ID); err != nil {
				t.Fatal(err)
			}
			stopped, err := rt.Workflow.Get(ctx, run.ID)
			if err != nil || stopped.State != workflow.Stopped {
				t.Fatalf("stop: %+v %v", stopped, err)
			}
			if has(api.issues["owner/repo"][0], "agent-needs-attention") {
				t.Fatalf("a run without leftovers flagged the issue: %+v", api.issues["owner/repo"][0].Labels)
			}
			if _, ok := api.leftoverComments[7]; ok {
				t.Fatalf("a run without leftovers was explained: %+v", api.leftoverComments)
			}
		})
	}
}

// TestStopKeepsIgnoredFilesOfAnUnownedRunInAnyState proves that a file Git
// ignores is work too: it never shows in the plain status, yet Stop must not
// delete it with the leftover worktree, and must name the leftovers.
func TestStopKeepsIgnoredFilesOfAnUnownedRunInAnyState(t *testing.T) {
	for _, state := range []workflow.State{workflow.NeedsAttention, workflow.Claiming} {
		t.Run(string(state), func(t *testing.T) {
			s, rt, api, _, _, _ := localFlow(t, successfulScript)
			ctx := context.Background()
			run := runAfterFailedPrepare(t, rt, "interrupted", state)
			prepared, err := managedgit.New(rt.Workspace).Prepare(ctx, managedgit.PrepareRequest{Repository: "owner/repo", RunID: run.ID, IssueNumber: 7})
			if err != nil {
				t.Fatal(err)
			}
			base := filepath.Join(rt.Workspace.Root, "repos", "owner-repo", "base")
			exclude := filepath.Join(base, ".git", "info", "exclude")
			if err := os.MkdirAll(filepath.Dir(exclude), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(exclude, []byte(".env\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(prepared.Path, ".env"), []byte("secret\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if status := gitCommand(t, prepared.Path, "status", "--porcelain"); status != "" {
				t.Fatalf("the file is not ignored: %q", status)
			}
			if err := s.Stop(ctx, run.ID); err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(filepath.Join(prepared.Path, ".env")); err != nil || string(data) != "secret\n" {
				t.Fatalf("the ignored file was deleted: %q, %v", data, err)
			}
			if !has(api.issues["owner/repo"][0], "agent-needs-attention") {
				t.Fatalf("the preserved leftovers did not flag the issue: %+v", api.issues["owner/repo"][0].Labels)
			}
			if comment, ok := api.leftoverComments[7]; !ok || comment.Path != prepared.Path {
				t.Fatalf("the preserved leftovers were not named on the issue: %+v", api.leftoverComments)
			}
		})
	}
}
