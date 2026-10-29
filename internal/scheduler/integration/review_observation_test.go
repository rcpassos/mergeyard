package integration_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestReviewStatusErrorStopsRestoresAndRequiresAttention(t *testing.T) {
	t.Parallel()

	for _, agent := range []string{"claude", "codex"} {
		for _, code := range []string{"phase.exit_invalid", "phase.exit_read_failed", "phase.process_identity"} {
			t.Run(agent+"/"+code, func(t *testing.T) {
				t.Parallel()

				_, runtime, api, _, cfg, r := testkit.TmuxPairingFlow(t, "claude", agent, testkit.DisputedReport)
				testkit.ReplacePhaseScript(t, cfg, workflow.Review, testkit.ReviewScript("printf tampered > feature.txt\n"+testkit.ApprovedReview))
				failed := &testkit.InvalidReviewStatus{Runner: r, Code: code}
				s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: failed})
				if err != nil {
					t.Fatal(err)
				}
				run := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Review)
				if failed.Stops != 1 || run.LastErrorCode != code || run.Review == nil || run.Review.Status != "failed" || !run.Review.Restored || !run.Review.Contaminated || run.Review.Accepted || run.ApprovedSHA != "" {
					t.Fatalf("status failure not safely rejected: stops=%d run=%+v", failed.Stops, run)
				}
				paths, err := filepath.Glob(filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", "*"))
				if err != nil || len(paths) != 1 {
					t.Fatalf("worktrees: %v %v", paths, err)
				}
				data, err := os.ReadFile(filepath.Join(paths[0], "feature.txt"))
				if err != nil || string(data) != "implemented\n" {
					t.Fatalf("review mutation retained: %q %v", data, err)
				}
				for range 3 {
					if err := s.Tick(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				saved, err := runtime.Workflow.Get(context.Background(), run.ID)
				if err != nil || saved.State != workflow.NeedsAttention || len(saved.ReviewHistory) != 1 || failed.Stops != 1 {
					t.Fatalf("repeated or stalled work: %+v %v", saved, err)
				}
			})
		}
	}
}

func TestReviewCanceledStatusLeavesAttemptAvailableForObservation(t *testing.T) {
	t.Parallel()

	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			t.Parallel()

			_, runtime, api, _, cfg, r := testkit.TmuxPairingFlow(t, "claude", agent, testkit.DisputedReport)
			testkit.ReplacePhaseScript(t, cfg, workflow.Review, testkit.ReviewScript(testkit.ApprovedReview))
			canceled := &testkit.InvalidReviewStatus{Runner: r, Code: "phase.canceled"}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: canceled})
			if err != nil {
				t.Fatal(err)
			}
			run := testkit.Finish(t, s, workflow.Active, workflow.Review)
			testkit.WaitForWithin(t, 30*time.Second, func() bool {
				err := s.Tick(context.Background())
				if err == nil {
					return false
				}
				var coded *fault.Error
				if !errors.As(err, &coded) || coded.Code != "phase.canceled" {
					t.Fatal(err)
				}
				return true
			})
			saved, err := runtime.Workflow.Get(context.Background(), run.ID)
			if err != nil || saved.State != workflow.Active || saved.Review.Status != "running" || saved.Review.Restored || canceled.Stops != 0 {
				t.Fatalf("cancellation aborted review: %+v %v", saved, err)
			}
			canceled.Code = ""
			done := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
			if !done.Review.Accepted || len(done.ReviewHistory) != 1 || canceled.Stops != 0 {
				t.Fatalf("cancellation duplicated review: %+v", done)
			}
		})
	}
}
