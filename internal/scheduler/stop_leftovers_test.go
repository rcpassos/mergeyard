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
	// The comment names the worktree relative to the workspace, never the
	// absolute path, which would expose the operator's filesystem layout.
	if want := filepath.ToSlash(filepath.Join("worktrees", "owner-repo", run.ID)); !ok || comment.Worktree != want {
		t.Fatalf("the preserved work was not named on the issue: %+v", api.leftoverComments)
	}
}

// unownedStates are the states a run without saved Git ownership can be in
// when a stop arrives: still PREPARING, NEEDS_ATTENTION after a failed Prepare,
// and CLAIMING once a Retry moved it on. In each, the preparation intent names
// the branch and the worktree an earlier attempt left.
var unownedStates = []workflow.State{workflow.Preparing, workflow.NeedsAttention, workflow.Claiming}

// runInState moves a run that started preparing into the state a stop finds it
// in. It saves no Git ownership.
func runInState(t *testing.T, rt *app.Runtime, id string, state workflow.State) workflow.Run {
	t.Helper()
	ctx := context.Background()
	run := preparingRun(t, rt, id)
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
func saveOwnership(t *testing.T, rt *app.Runtime, run workflow.Run, prepared managedgit.Run) {
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

// TestStopReleasesLeftoversOfAnUnownedRunInAnyState proves that the release
// does not depend on the run still preparing: a Prepare that failed leaves the
// run in NEEDS_ATTENTION, and a Retry of it leaves the run in CLAIMING, both
// with the earlier attempt's branch and worktree still named by its intent.
// The next run for the issue prepares instead of refusing them.
func TestStopReleasesLeftoversOfAnUnownedRunInAnyState(t *testing.T) {
	for _, state := range unownedStates {
		t.Run(string(state), func(t *testing.T) {
			s, rt, api, _, _, _ := localFlow(t, successfulScript)
			ctx := context.Background()
			run := runInState(t, rt, "interrupted", state)
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
	for _, state := range unownedStates {
		t.Run(string(state), func(t *testing.T) {
			s, rt, api, _, _, _ := localFlow(t, successfulScript)
			ctx := context.Background()
			run := runInState(t, rt, "interrupted", state)
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
			if want := filepath.ToSlash(filepath.Join("worktrees", "owner-repo", run.ID)); comment.Branch != "mergeyard/issue-7" || comment.Worktree != want || filepath.IsAbs(comment.Worktree) {
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
		})
	}
}

// TestStopKeepsTheWorktreeOfASavedOwnershipInAnyState proves that a run that
// saved its ownership keeps its branch and worktree on Stop, whatever state a
// failure or a Retry moved it to.
func TestStopKeepsTheWorktreeOfASavedOwnershipInAnyState(t *testing.T) {
	for _, state := range unownedStates {
		t.Run(string(state), func(t *testing.T) {
			stopSavedOwnership(t, state)
		})
	}
}

func stopSavedOwnership(t *testing.T, state workflow.State) {
	t.Helper()
	s, rt, api, _, _, _ := localFlow(t, successfulScript)
	ctx := context.Background()
	run := runInState(t, rt, "owned", state)
	prepared, err := managedgit.New(rt.Workspace).Prepare(ctx, managedgit.PrepareRequest{Repository: "owner/repo", RunID: run.ID, IssueNumber: 7})
	if err != nil {
		t.Fatal(err)
	}
	saveOwnership(t, rt, run, prepared)
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
	for _, state := range unownedStates {
		t.Run(string(state), func(t *testing.T) {
			s, rt, api, _, _, _ := localFlow(t, successfulScript)
			ctx := context.Background()
			run := runInState(t, rt, "untouched", state)
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
	for _, state := range unownedStates {
		t.Run(string(state), func(t *testing.T) {
			s, rt, api, _, _, _ := localFlow(t, successfulScript)
			ctx := context.Background()
			run := runInState(t, rt, "interrupted", state)
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
			comment, ok := api.leftoverComments[7]
			if want := filepath.ToSlash(filepath.Join("worktrees", "owner-repo", run.ID)); !ok || comment.Worktree != want {
				t.Fatalf("the preserved leftovers were not named on the issue: %+v", api.leftoverComments)
			}
		})
	}
}

// TestStopCompletesWhenTheManagedBaseIsGone proves that a stop does not wedge
// on a claim that outlives its managed base: the run reaches STOPPED, and the
// leftovers it cannot verify are named on the issue.
func TestStopCompletesWhenTheManagedBaseIsGone(t *testing.T) {
	for _, state := range unownedStates {
		t.Run(string(state), func(t *testing.T) {
			s, rt, api, _, _, _ := localFlow(t, successfulScript)
			ctx := context.Background()
			run := runInState(t, rt, "interrupted", state)
			prepared, err := managedgit.New(rt.Workspace).Prepare(ctx, managedgit.PrepareRequest{Repository: "owner/repo", RunID: run.ID, IssueNumber: 7})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(filepath.Join(rt.Workspace.Root, "repos", "owner-repo", "base")); err != nil {
				t.Fatal(err)
			}
			if err := s.Stop(ctx, run.ID); err != nil {
				t.Fatal(err)
			}
			stopped, err := rt.Workflow.Get(ctx, run.ID)
			if err != nil || stopped.State != workflow.Stopped {
				t.Fatalf("stop: %+v %v", stopped, err)
			}
			if _, err := os.Stat(prepared.Path); err != nil {
				t.Fatalf("the worktree was removed without its base: %v", err)
			}
			if !has(api.issues["owner/repo"][0], "agent-needs-attention") {
				t.Fatalf("the unverifiable leftovers did not flag the issue: %+v", api.issues["owner/repo"][0].Labels)
			}
			if comment, ok := api.leftoverComments[7]; !ok || !comment.Unverified {
				t.Fatalf("the unverifiable leftovers were not named on the issue: %+v", api.leftoverComments)
			}
		})
	}
}

// requireStoppedUnverified proves that a stop finished on leftovers it could
// not check: the run is STOPPED, nothing was deleted, the issue is flagged, and
// the comment names the run's branch and worktree as unverified.
func requireStoppedUnverified(t *testing.T, rt *app.Runtime, api *fakeGitHub, run workflow.Run, prepared managedgit.Run) {
	t.Helper()
	stopped, err := rt.Workflow.Get(context.Background(), run.ID)
	if err != nil || stopped.State != workflow.Stopped {
		t.Fatalf("stop: %+v %v", stopped, err)
	}
	if _, err := os.Stat(prepared.Path); err != nil {
		t.Fatalf("the unverified worktree was removed: %v", err)
	}
	if !has(api.issues["owner/repo"][0], "agent-needs-attention") {
		t.Fatalf("the unverified leftovers did not flag the issue: %+v", api.issues["owner/repo"][0].Labels)
	}
	comment, ok := api.leftoverComments[7]
	if want := filepath.ToSlash(filepath.Join("worktrees", "owner-repo", run.ID)); !ok || !comment.Unverified || comment.Branch != "mergeyard/issue-7" || comment.Worktree != want {
		t.Fatalf("the unverified leftovers were not named on the issue: %+v", api.leftoverComments)
	}
}

// TestStopCompletesWhenTheLeftoversCannotBeChecked proves that a stop does not
// wedge on a claim that a crash or a person left empty or corrupt, or on a
// managed base replaced by another clone: the run reaches STOPPED and the
// leftovers are kept and named as unverified.
func TestStopCompletesWhenTheLeftoversCannotBeChecked(t *testing.T) {
	base := func(rt *app.Runtime) string { return filepath.Join(rt.Workspace.Root, "repos", "owner-repo", "base") }
	claim := func(content string) func(*testing.T, *app.Runtime, string) {
		return func(t *testing.T, rt *app.Runtime, remote string) {
			path, err := managedgit.PrepareIntentPath(rt.Workspace.Root, "owner/repo", "interrupted")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, breakIt := range map[string]func(*testing.T, *app.Runtime, string){
		"empty claim":   claim(""),
		"corrupt claim": claim(`{"id":"interrupted",`),
		"replaced base": func(t *testing.T, rt *app.Runtime, remote string) {
			if err := os.RemoveAll(base(rt)); err != nil {
				t.Fatal(err)
			}
			gitCommand(t, rt.Workspace.Root, "clone", "--quiet", remote, base(rt))
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, rt, api, remote, _, _ := localFlow(t, successfulScript)
			ctx := context.Background()
			run := runInState(t, rt, "interrupted", workflow.Preparing)
			prepared, err := managedgit.New(rt.Workspace).Prepare(ctx, managedgit.PrepareRequest{Repository: "owner/repo", RunID: run.ID, IssueNumber: 7})
			if err != nil {
				t.Fatal(err)
			}
			breakIt(t, rt, remote)
			if err := s.Stop(ctx, run.ID); err != nil {
				t.Fatal(err)
			}
			requireStoppedUnverified(t, rt, api, run, prepared)
		})
	}
}

// TestStopCompletesWhenGitHangsOnTheLeftovers proves that a Git check that
// exceeds its deadline does not leave the stop pending: every attempt would
// hang the same way, so the first one finishes the stop on unverified
// leftovers, and another stop finds the run already STOPPED.
func TestStopCompletesWhenGitHangsOnTheLeftovers(t *testing.T) {
	_, rt, api, _, cfg, r := localFlow(t, successfulScript)
	ctx := context.Background()
	run := runInState(t, rt, "interrupted", workflow.Preparing)
	prepared, err := managedgit.New(rt.Workspace).Prepare(ctx, managedgit.PrepareRequest{Repository: "owner/repo", RunID: run.ID, IssueNumber: 7})
	if err != nil {
		t.Fatal(err)
	}
	installGitWrapper(t, "status", "exec /bin/sleep 600")
	short := managedgit.NewWithTimeouts(rt.Workspace, managedgit.Timeouts{Metadata: time.Second, Worktree: time.Second, Clone: time.Second, Fetch: time.Second, Push: time.Second})
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Git: short, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(ctx, run.ID); err != nil {
		t.Fatalf("a hung Git check failed the stop: %v", err)
	}
	requireStoppedUnverified(t, rt, api, run, prepared)
	if err := s.Stop(ctx, run.ID); err != nil {
		t.Fatalf("a repeated stop failed: %v", err)
	}
}
