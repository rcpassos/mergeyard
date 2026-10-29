package testkit

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type ReviewProcessInput struct {
	Config            config.Config
	Workspace, Socket string
	PR                *github.PullRequest
	KillDuringRestore bool
	TakeoverCrash     string
	KillDuringFix     string
	RecoveryPhase     workflow.Phase
	RecoveryCrash     string
	Remote            string
}
type InterruptedRestoreGit struct{ *managedgit.Manager }

func (g InterruptedRestoreGit) RestoreReview(ctx context.Context, run managedgit.Run, snapshot managedgit.ReviewSnapshot) error {
	if err := g.Manager.RestoreReview(ctx, run, snapshot); err != nil {
		return err
	}
	fmt.Println("review-control-plane-ready")
	// The parent kills the process with restored Git but a pending journal.
	select {}
}

func WaitFor(t *testing.T, condition func() bool) {
	t.Helper()
	WaitForWithin(t, 5*time.Second, condition)
}

func WaitForWithin(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for process artifact")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
