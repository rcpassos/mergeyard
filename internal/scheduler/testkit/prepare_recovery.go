package testkit

import (
	"context"
	"testing"

	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
)

// prepareIntentPath returns the file where preparation records that this run
// created its branch and worktree.
func PrepareIntentPath(t *testing.T, root, runID string) string {
	t.Helper()
	path, err := managedgit.PrepareIntentPath(root, "owner/repo", runID)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// failingConfirmGit fails the first preparation-intent releases, so a saved
// ownership can be left with the intent that recorded it.
type FailingConfirmGit struct {
	*managedgit.Manager
	Failures int
}

func (g *FailingConfirmGit) ConfirmOwnership(ctx context.Context, run managedgit.Run) error {
	if g.Failures > 0 {
		g.Failures--
		return &fault.Error{Code: "git.worktree", Message: "the preparation intent could not be released"}
	}
	return g.Manager.ConfirmOwnership(ctx, run)
}
