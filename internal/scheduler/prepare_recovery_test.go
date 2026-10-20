package scheduler_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// prepareIntentPath returns the file where preparation records that this run
// created its branch and worktree.
func prepareIntentPath(t *testing.T, root, runID string) string {
	t.Helper()
	path, err := managedgit.PrepareIntentPath(root, "owner/repo", runID)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPreparedRunReleasesItsIntentOnceOwnershipIsSaved(t *testing.T) {
	s, rt, _, _, _, _ := localFlow(t, successfulScript)
	run := finish(t, s, workflow.Active, workflow.Review)
	if _, err := os.Stat(prepareIntentPath(t, rt.Workspace.Root, run.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the preparation intent outlived the saved ownership: %v", err)
	}
}

// TestPrepareRecoversAfterOwnershipSaveFails covers the gap between creating
// the worktree and saving the run's Git ownership. The branch and worktree
// exist, so the next attempt must recover them instead of refusing them.
func TestPrepareRecoversAfterOwnershipSaveFails(t *testing.T) {
	s, rt, _, _, _, _ := localFlow(t, successfulScript)
	// The ownership write fails after the worktree was created.
	if _, err := rt.DB.Exec(`CREATE TRIGGER fail_ownership BEFORE UPDATE OF branch ON runs BEGIN SELECT RAISE(ABORT, 'ownership save failed'); END`); err != nil {
		t.Fatal(err)
	}
	failed := finish(t, s, workflow.NeedsAttention, "")
	if failed.LastErrorCode != "internal.scheduler" || !strings.Contains(failed.LastErrorMessage, "ownership save failed") {
		t.Fatalf("the ownership save failure was not reported: %+v", failed)
	}
	intent := prepareIntentPath(t, rt.Workspace.Root, failed.ID)
	if _, err := os.Stat(intent); err != nil {
		t.Fatalf("the recovery intent did not survive the failed ownership save: %v", err)
	}
	if _, err := rt.DB.Exec(`DROP TRIGGER fail_ownership`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Retry(context.Background(), failed.ID); err != nil {
		t.Fatal(err)
	}
	recovered := finish(t, s, workflow.Active, workflow.Review)
	if recovered.LastErrorCode == "git.branch_conflict" {
		t.Fatalf("the retry refused the run's own worktree: %+v", recovered)
	}
	if _, err := os.Stat(intent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the preparation intent outlived the saved ownership: %v", err)
	}
}

// failingConfirmGit fails the first preparation-intent releases, so a saved
// ownership can be left with the intent that recorded it.
type failingConfirmGit struct {
	*managedgit.Manager
	failures int
}

func (g *failingConfirmGit) ConfirmOwnership(ctx context.Context, run managedgit.Run) error {
	if g.failures > 0 {
		g.failures--
		return &fault.Error{Code: "git.worktree", Message: "the preparation intent could not be released"}
	}
	return g.Manager.ConfirmOwnership(ctx, run)
}

// TestRetryReleasesTheIntentAfterAFailedConfirmation proves that a run whose
// ownership was saved but whose intent release failed removes the stale intent
// when it resumes, instead of carrying it through the recovered run.
func TestRetryReleasesTheIntentAfterAFailedConfirmation(t *testing.T) {
	_, rt, api, _, cfg, r := localFlow(t, successfulScript)
	git := &failingConfirmGit{Manager: managedgit.New(rt.Workspace), failures: 1}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r, Git: git})
	if err != nil {
		t.Fatal(err)
	}
	failed := finish(t, s, workflow.NeedsAttention, "")
	if failed.LastErrorCode != "git.worktree" {
		t.Fatalf("the failed intent release was not reported: %+v", failed)
	}
	intent := prepareIntentPath(t, rt.Workspace.Root, failed.ID)
	if _, err := os.Stat(intent); err != nil {
		t.Fatalf("the intent did not survive the failed release: %v", err)
	}
	if _, err := s.Retry(context.Background(), failed.ID); err != nil {
		t.Fatal(err)
	}
	recovered := finish(t, s, workflow.Active, workflow.Review)
	if recovered.LastErrorCode == "git.branch_conflict" {
		t.Fatalf("the retry refused the run's own worktree: %+v", recovered)
	}
	if _, err := os.Stat(intent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the preparation intent outlived the resumed run: %v", err)
	}
}
