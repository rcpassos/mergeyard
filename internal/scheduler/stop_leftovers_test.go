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
	if blocked.LastErrorCode != "git.branch_conflict" || !strings.Contains(blocked.LastErrorMessage, "mergeyard/issue-7") {
		t.Fatalf("the next run did not name the blocker: %+v", blocked)
	}
}
