package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestHungGitPublicationTimesOutAndTheNextTickProceeds(t *testing.T) {
	_, rt, api, _, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	testkit.InstallHangingGit(t, "push")
	// Local preparation and fetch fit their budgets; the push exhausts its own.
	// The budget leaves headroom for slow CI runners while staying far below
	// the test's own deadlines.
	manager := testkit.TestGitManagerWithTimeouts(rt.Workspace, managedgit.Timeouts{
		Metadata: 30 * time.Second, Worktree: 30 * time.Second, Clone: 30 * time.Second, Fetch: 6 * time.Second, Push: 6 * time.Second,
	})
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r, Git: manager})
	if err != nil {
		t.Fatal(err)
	}
	failed := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
	if failed.LastErrorCode != "git.timeout" || !strings.Contains(failed.LastErrorMessage, "push timed out") || !strings.Contains(failed.LastErrorMessage, "push budget") {
		t.Fatalf("hung publication did not end as an explained, coded timeout: %+v", failed)
	}
	if failed.FixHistory != nil || failed.ReviewRound != 0 {
		t.Fatalf("timeout advanced the run: %+v", failed)
	}
	// The hung command released the tick: the next tick reconciles attention.
	start := time.Now()
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("next tick waited %s on the hung Git command", elapsed)
	}
}
