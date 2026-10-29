package integration_test

import (
	"context"
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
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// TestStopPreparingRunReleasesAnInterruptedCreation proves that stopping a run
// whose worktree creation was interrupted removes the partial worktree and the
// branch it left, so the next run for the same issue prepares.
func TestStopPreparingRunReleasesAnInterruptedCreation(t *testing.T) {
	t.Parallel()

	s, rt, api, _, _, _ := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	ctx := context.Background()
	run := testkit.PreparingRun(t, rt, "interrupted")
	path := testkit.InterruptPreparingCheckout(t, rt, testkit.TestGitManager(rt.Workspace), run)
	if err := s.Stop(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the interrupted worktree survived the stop: %v", err)
	}
	base := filepath.Join(rt.Workspace.Root, "repos", "owner-repo", "base")
	if branch := testkit.GitCommand(t, base, "branch", "--list", "mergeyard/issue-7"); branch != "" {
		t.Fatalf("the interrupted branch survived the stop: %s", branch)
	}
	// The issue is queued again, and the next run prepares it.
	api.Issues["owner/repo"][0].Labels = []github.Label{{Name: "ready-for-agent"}}
	next := testkit.WaitForRun(t, s, workflow.Active, workflow.Review)
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
	t.Parallel()

	s, rt, api, _, _, _ := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	ctx := context.Background()
	run := testkit.PreparingRun(t, rt, "interrupted")
	git := testkit.TestGitManager(rt.Workspace)
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
	issue := api.Issues["owner/repo"][0]
	if !testkit.Has(issue, "agent-needs-attention") {
		t.Fatalf("the preserved work did not flag the issue: %+v", issue.Labels)
	}
	comment, ok := api.LeftoverComments[7]
	// The comment names the worktree relative to the workspace, never the
	// absolute path, which would expose the operator's filesystem layout.
	if want := filepath.ToSlash(filepath.Join("worktrees", "owner-repo", run.ID)); !ok || comment.Worktree != want {
		t.Fatalf("the preserved work was not named on the issue: %+v", api.LeftoverComments)
	}
}

// TestStopPreparingRunKeepsOrReleasesTheSubmodulesOfAnInterruptedCheckout
// proves that a stop keeps a person's work inside a submodule of a clean,
// unlocked leftover checkout and names the leftovers, while empty submodule
// directories still let it discard the checkout. The initialized submodule's
// edit is hidden by .gitmodules, so it also proves the status check overrides
// that setting.
func TestStopPreparingRunKeepsOrReleasesTheSubmodulesOfAnInterruptedCheckout(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		ignore     string
		initialize bool
		file       string
		content    string
	}{
		{name: "empty submodule"},
		{name: "file in uninitialized submodule", file: "human.txt", content: "uncommitted human work\n"},
		{name: "edit in initialized submodule hidden by ignore", ignore: "all", initialize: true, file: "module.txt", content: "edited by a person\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s, rt, api, remote, _, _ := testkit.TmuxFlow(t, testkit.SuccessfulScript)
			testkit.SeedRemoteSubmodule(t, remote, tc.ignore)
			ctx := context.Background()
			run := testkit.PreparingRun(t, rt, "interrupted")
			path := testkit.InterruptPreparingCheckout(t, rt, testkit.TestGitManager(rt.Workspace), run)
			if tc.initialize {
				testkit.GitCommand(t, path, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--", "module")
			}
			if tc.file != "" {
				if err := os.MkdirAll(filepath.Join(path, "module"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "module", tc.file), []byte(tc.content), 0600); err != nil {
					t.Fatal(err)
				}
				if status := testkit.GitCommand(t, path, "status", "--porcelain", "--untracked-files=all", "--ignored"); status != "" {
					t.Fatalf("the fixture work is not hidden from status: %q", status)
				}
			}
			if err := s.Stop(ctx, run.ID); err != nil {
				t.Fatal(err)
			}
			base := filepath.Join(rt.Workspace.Root, "repos", "owner-repo", "base")
			if tc.file != "" {
				if data, err := os.ReadFile(filepath.Join(path, "module", tc.file)); err != nil || string(data) != tc.content {
					t.Fatalf("the work was deleted: %q, %v", data, err)
				}
				if !testkit.Has(api.Issues["owner/repo"][0], "agent-needs-attention") {
					t.Fatalf("the preserved work did not flag the issue: %+v", api.Issues["owner/repo"][0].Labels)
				}
				comment, ok := api.LeftoverComments[7]
				if want := filepath.ToSlash(filepath.Join("worktrees", "owner-repo", run.ID)); !ok || comment.Unverified || comment.Worktree != want {
					t.Fatalf("the preserved work was not named on the issue: %+v", api.LeftoverComments)
				}
				if branch := testkit.GitCommand(t, base, "branch", "--list", "mergeyard/issue-7"); branch == "" {
					t.Fatal("the branch of the preserved work was deleted")
				}
				return
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the interrupted worktree survived the stop: %v", err)
			}
			if branch := testkit.GitCommand(t, base, "branch", "--list", "mergeyard/issue-7"); branch != "" {
				t.Fatalf("the interrupted branch survived the stop: %s", branch)
			}
			if testkit.Has(api.Issues["owner/repo"][0], "agent-needs-attention") {
				t.Fatalf("the released leftovers flagged the issue: %+v", api.Issues["owner/repo"][0].Labels)
			}
			if _, ok := api.LeftoverComments[7]; ok {
				t.Fatalf("released leftovers were explained on the issue: %+v", api.LeftoverComments)
			}
		})
	}
}

// TestStopReleasesLeftoversOfAnUnownedRunInAnyState proves that the release
// does not depend on the run still preparing: a Prepare that failed leaves the
// run in NEEDS_ATTENTION, and a Retry of it leaves the run in CLAIMING, both
// with the earlier attempt's branch and worktree still named by its intent.
// The next run for the issue prepares instead of refusing them.
func TestStopReleasesLeftoversOfAnUnownedRunInAnyState(t *testing.T) {
	t.Parallel()

	for _, state := range testkit.UnownedStates {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()

			s, rt, api, _, _, _ := testkit.TmuxFlow(t, testkit.SuccessfulScript)
			ctx := context.Background()
			run := testkit.RunInState(t, rt, "interrupted", state)
			prepared, err := testkit.TestGitManager(rt.Workspace).Prepare(ctx, managedgit.PrepareRequest{Repository: "owner/repo", RunID: run.ID, IssueNumber: 7})
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
			if branch := testkit.GitCommand(t, base, "branch", "--list", "mergeyard/issue-7"); branch != "" {
				t.Fatalf("the leftover branch survived the stop: %s", branch)
			}
			if testkit.Has(api.Issues["owner/repo"][0], "agent-needs-attention") {
				t.Fatalf("the released leftovers flagged the issue: %+v", api.Issues["owner/repo"][0].Labels)
			}
			if _, ok := api.LeftoverComments[7]; ok {
				t.Fatalf("released leftovers were explained on the issue: %+v", api.LeftoverComments)
			}
			// The issue is queued again, and the next run prepares it.
			api.Issues["owner/repo"][0].Labels = []github.Label{{Name: "ready-for-agent"}}
			next := testkit.WaitForRun(t, s, workflow.Active, workflow.Review)
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
	t.Parallel()

	for _, state := range testkit.UnownedStates {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()

			s, rt, api, _, _, _ := testkit.TmuxFlow(t, testkit.SuccessfulScript)
			ctx := context.Background()
			run := testkit.RunInState(t, rt, "interrupted", state)
			prepared, err := testkit.TestGitManager(rt.Workspace).Prepare(ctx, managedgit.PrepareRequest{Repository: "owner/repo", RunID: run.ID, IssueNumber: 7})
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
			if !testkit.Has(api.Issues["owner/repo"][0], "agent-needs-attention") {
				t.Fatalf("the preserved leftovers did not flag the issue: %+v", api.Issues["owner/repo"][0].Labels)
			}
			comment, ok := api.LeftoverComments[7]
			if !ok {
				t.Fatalf("the preserved leftovers were not named on the issue: %+v", api.LeftoverComments)
			}
			if want := filepath.ToSlash(filepath.Join("worktrees", "owner-repo", run.ID)); comment.Branch != "mergeyard/issue-7" || comment.Worktree != want || filepath.IsAbs(comment.Worktree) {
				t.Fatalf("the explanation does not name the leftovers: %+v", comment)
			}
			// The next run for the same issue is told what blocks it.
			api.Issues["owner/repo"][0].Labels = []github.Label{{Name: "ready-for-agent"}}
			blocked := testkit.WaitForRun(t, s, workflow.NeedsAttention, "")
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
	t.Parallel()

	for _, state := range testkit.UnownedStates {
		t.Run(string(state), func(t *testing.T) {
			testkit.StopSavedOwnership(t, state)
		})
	}
}

// TestStopKeepsIgnoredFilesOfAnUnownedRunInAnyState proves that a file Git
// ignores is work too: it never shows in the plain status, yet Stop must not
// delete it with the leftover worktree, and must name the leftovers.
func TestStopKeepsIgnoredFilesOfAnUnownedRunInAnyState(t *testing.T) {
	t.Parallel()

	for _, state := range testkit.UnownedStates {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()

			s, rt, api, _, _, _ := testkit.TmuxFlow(t, testkit.SuccessfulScript)
			ctx := context.Background()
			run := testkit.RunInState(t, rt, "interrupted", state)
			prepared, err := testkit.TestGitManager(rt.Workspace).Prepare(ctx, managedgit.PrepareRequest{Repository: "owner/repo", RunID: run.ID, IssueNumber: 7})
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
			if status := testkit.GitCommand(t, prepared.Path, "status", "--porcelain"); status != "" {
				t.Fatalf("the file is not ignored: %q", status)
			}
			if err := s.Stop(ctx, run.ID); err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(filepath.Join(prepared.Path, ".env")); err != nil || string(data) != "secret\n" {
				t.Fatalf("the ignored file was deleted: %q, %v", data, err)
			}
			if !testkit.Has(api.Issues["owner/repo"][0], "agent-needs-attention") {
				t.Fatalf("the preserved leftovers did not flag the issue: %+v", api.Issues["owner/repo"][0].Labels)
			}
			comment, ok := api.LeftoverComments[7]
			if want := filepath.ToSlash(filepath.Join("worktrees", "owner-repo", run.ID)); !ok || comment.Worktree != want {
				t.Fatalf("the preserved leftovers were not named on the issue: %+v", api.LeftoverComments)
			}
		})
	}
}

// TestStopCompletesWhenTheManagedBaseIsGone proves that a stop does not wedge
// on a claim that outlives its managed base: the run reaches STOPPED, and the
// leftovers it cannot verify are named on the issue.
func TestStopCompletesWhenTheManagedBaseIsGone(t *testing.T) {
	t.Parallel()

	for _, state := range testkit.UnownedStates {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()

			s, rt, api, _, _, _ := testkit.TmuxFlow(t, testkit.SuccessfulScript)
			ctx := context.Background()
			run := testkit.RunInState(t, rt, "interrupted", state)
			prepared, err := testkit.TestGitManager(rt.Workspace).Prepare(ctx, managedgit.PrepareRequest{Repository: "owner/repo", RunID: run.ID, IssueNumber: 7})
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
			if !testkit.Has(api.Issues["owner/repo"][0], "agent-needs-attention") {
				t.Fatalf("the unverifiable leftovers did not flag the issue: %+v", api.Issues["owner/repo"][0].Labels)
			}
			if comment, ok := api.LeftoverComments[7]; !ok || !comment.Unverified {
				t.Fatalf("the unverifiable leftovers were not named on the issue: %+v", api.LeftoverComments)
			}
		})
	}
}

// TestStopCompletesWhenTheLeftoversCannotBeChecked proves that a stop does not
// wedge on a claim that a crash or a person left empty or corrupt, or on a
// managed base replaced by another clone: the run reaches STOPPED and the
// leftovers are kept and named as unverified.
func TestStopCompletesWhenTheLeftoversCannotBeChecked(t *testing.T) {
	t.Parallel()

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
			testkit.GitCommand(t, rt.Workspace.Root, "clone", "--quiet", remote, base(rt))
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			s, rt, api, remote, _, _ := testkit.TmuxFlow(t, testkit.SuccessfulScript)
			ctx := context.Background()
			run := testkit.RunInState(t, rt, "interrupted", workflow.Preparing)
			prepared, err := testkit.TestGitManager(rt.Workspace).Prepare(ctx, managedgit.PrepareRequest{Repository: "owner/repo", RunID: run.ID, IssueNumber: 7})
			if err != nil {
				t.Fatal(err)
			}
			breakIt(t, rt, remote)
			if err := s.Stop(ctx, run.ID); err != nil {
				t.Fatal(err)
			}
			testkit.RequireStoppedUnverified(t, rt, api, run, prepared)
		})
	}
}

// TestStopClearsLeftoversThatARetryRemoved proves that a retry which verifies
// and removes the leftovers a failed Stop recorded clears that record: the
// stopped run names no gone artifacts and does not flag the issue.
func TestStopClearsLeftoversThatARetryRemoved(t *testing.T) {
	s, rt, api, _, _, _ := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	ctx := context.Background()
	run, prepared, gate := testkit.StoppedWithUnverifiedLeftovers(t, s, rt, api)
	if err := os.Remove(gate); err != nil {
		t.Fatal(err)
	}
	api.Mutate = nil
	if err := s.Stop(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	stopped, err := rt.Workflow.Get(ctx, run.ID)
	if err != nil || stopped.State != workflow.Stopped {
		t.Fatalf("stop: %+v %v", stopped, err)
	}
	testkit.RequireNoRecordedLeftovers(t, rt, run.ID)
	if _, err := os.Stat(prepared.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the removed leftovers survive in the record: %v", err)
	}
	if testkit.Has(api.Issues["owner/repo"][0], "agent-needs-attention") {
		t.Fatalf("leftovers that are gone flagged the issue: %+v", api.Issues["owner/repo"][0].Labels)
	}
	if _, ok := api.LeftoverComments[7]; ok {
		t.Fatalf("leftovers that are gone were explained on the issue: %+v", api.LeftoverComments)
	}
}

// TestStopKeepsLeftoversThatARetryStillFinds proves that a retry which still
// finds work keeps the leftovers recorded, flags the issue and explains them,
// without adding another stop-leftovers event.
func TestStopKeepsLeftoversThatARetryStillFinds(t *testing.T) {
	s, rt, api, _, _, _ := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	ctx := context.Background()
	run, prepared, gate := testkit.StoppedWithUnverifiedLeftovers(t, s, rt, api)
	if err := os.WriteFile(filepath.Join(prepared.Path, "human.txt"), []byte("work\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(gate); err != nil {
		t.Fatal(err)
	}
	api.Mutate = nil
	if err := s.Stop(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	stopped, err := rt.Workflow.Get(ctx, run.ID)
	if err != nil || stopped.State != workflow.Stopped {
		t.Fatalf("stop: %+v %v", stopped, err)
	}
	testkit.RequireRecordedLeftovers(t, rt, run.ID, prepared)
	if !testkit.Has(api.Issues["owner/repo"][0], "agent-needs-attention") {
		t.Fatalf("the preserved leftovers did not flag the issue: %+v", api.Issues["owner/repo"][0].Labels)
	}
	comment, ok := api.LeftoverComments[7]
	if want := filepath.ToSlash(filepath.Join("worktrees", "owner-repo", run.ID)); !ok || comment.Branch != prepared.Branch || comment.Worktree != want {
		t.Fatalf("the preserved leftovers were not named on the issue: %+v", api.LeftoverComments)
	}
	if got := testkit.LeftoverEvents(t, rt, run.ID); got != 1 {
		t.Fatalf("the retry recorded %d stop-leftovers events, want 1", got)
	}
}

// TestRepeatedStopRecordsTheSameLeftoversOnce proves that retries that keep
// the same leftovers add one stop-leftovers event, not one per attempt.
func TestRepeatedStopRecordsTheSameLeftoversOnce(t *testing.T) {
	t.Parallel()

	s, rt, api, _, _, _ := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	ctx := context.Background()
	run := testkit.PreparingRun(t, rt, "interrupted")
	prepared, err := testkit.TestGitManager(rt.Workspace).Prepare(ctx, managedgit.PrepareRequest{Repository: "owner/repo", RunID: run.ID, IssueNumber: 7})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prepared.Path, "human.txt"), []byte("work\n"), 0600); err != nil {
		t.Fatal(err)
	}
	testkit.FailLabelRemovals(api, 2)
	for attempt := 1; attempt <= 2; attempt++ {
		if err := s.Stop(ctx, run.ID); err == nil {
			t.Fatalf("attempt %d did not fail the label removal", attempt)
		}
		if got := testkit.LeftoverEvents(t, rt, run.ID); got != 1 {
			t.Fatalf("attempt %d recorded %d stop-leftovers events, want 1", attempt, got)
		}
	}
	api.Mutate = nil
	if err := s.Stop(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	testkit.RequireRecordedLeftovers(t, rt, run.ID, prepared)
	if got := testkit.LeftoverEvents(t, rt, run.ID); got != 1 {
		t.Fatalf("the finished stop recorded %d stop-leftovers events, want 1", got)
	}
}

// TestStopCompletesWhenGitHangsOnTheLeftovers proves that a Git check that
// exceeds its deadline does not leave the stop pending: every attempt would
// hang the same way, so the first one finishes the stop on unverified
// leftovers, and another stop finds the run already STOPPED.
func TestStopCompletesWhenGitHangsOnTheLeftovers(t *testing.T) {
	_, rt, api, _, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	ctx := context.Background()
	run := testkit.RunInState(t, rt, "interrupted", workflow.Preparing)
	prepared, err := testkit.TestGitManager(rt.Workspace).Prepare(ctx, managedgit.PrepareRequest{Repository: "owner/repo", RunID: run.ID, IssueNumber: 7})
	if err != nil {
		t.Fatal(err)
	}
	testkit.InstallGitWrapper(t, "status", "exec /bin/sleep 600")
	short := testkit.TestGitManagerWithTimeouts(rt.Workspace, managedgit.Timeouts{Metadata: time.Second, Worktree: time.Second, Clone: time.Second, Fetch: time.Second, Push: time.Second})
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Git: short, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(ctx, run.ID); err != nil {
		t.Fatalf("a hung Git check failed the stop: %v", err)
	}
	testkit.RequireStoppedUnverified(t, rt, api, run, prepared)
	if err := s.Stop(ctx, run.ID); err != nil {
		t.Fatalf("a repeated stop failed: %v", err)
	}
}
