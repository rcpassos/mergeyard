package scheduler_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// Keep real process ownership and stopping; only the status observation fails.
type invalidReviewStatus struct {
	runner.Runner
	code  string
	stops int
}

func (r *invalidReviewStatus) SessionStatus(ctx context.Context, ref runner.SessionRef) (runner.SessionStatus, error) {
	status, err := r.Runner.SessionStatus(ctx, ref)
	if err == nil && r.code != "" && strings.HasPrefix(filepath.Base(ref.PhaseDir), "review-") && status.State == runner.SessionExited {
		return runner.SessionStatus{}, &fault.Error{Code: r.code, Message: "review process metadata is unreadable"}
	}
	return status, err
}
func (r *invalidReviewStatus) StopSession(ctx context.Context, ref runner.SessionRef) error {
	if strings.HasPrefix(filepath.Base(ref.PhaseDir), "review-") {
		r.stops++
	}
	return r.Runner.StopSession(ctx, ref)
}

func TestReviewStatusErrorStopsRestoresAndRequiresAttention(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		for _, code := range []string{"phase.exit_invalid", "phase.exit_read_failed", "phase.process_identity"} {
			t.Run(agent+"/"+code, func(t *testing.T) {
				_, runtime, api, _, cfg, r := pairingFlow(t, "claude", agent, disputedReport)
				replacePhaseScript(t, cfg, workflow.Review, reviewScript("printf tampered > feature.txt\n"+approvedReview))
				failed := &invalidReviewStatus{Runner: r, code: code}
				s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: failed})
				if err != nil {
					t.Fatal(err)
				}
				run := finish(t, s, workflow.NeedsAttention, workflow.Review)
				if failed.stops != 1 || run.LastErrorCode != code || run.Review == nil || run.Review.Status != "failed" || !run.Review.Restored || !run.Review.Contaminated || run.Review.Accepted || run.ApprovedSHA != "" {
					t.Fatalf("status failure not safely rejected: stops=%d run=%+v", failed.stops, run)
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
				if err != nil || saved.State != workflow.NeedsAttention || len(saved.ReviewHistory) != 1 || failed.stops != 1 {
					t.Fatalf("repeated or stalled work: %+v %v", saved, err)
				}
			})
		}
	}
}

func TestReviewCanceledStatusLeavesAttemptAvailableForObservation(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			_, runtime, api, _, cfg, r := pairingFlow(t, "claude", agent, disputedReport)
			replacePhaseScript(t, cfg, workflow.Review, reviewScript(approvedReview))
			canceled := &invalidReviewStatus{Runner: r, code: "phase.canceled"}
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: canceled})
			if err != nil {
				t.Fatal(err)
			}
			run := finish(t, s, workflow.Active, workflow.Review)
			waitForWithin(t, 30*time.Second, func() bool {
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
			if err != nil || saved.State != workflow.Active || saved.Review.Status != "running" || saved.Review.Restored || canceled.stops != 0 {
				t.Fatalf("cancellation aborted review: %+v %v", saved, err)
			}
			canceled.code = ""
			done := finish(t, s, workflow.WaitingForCI, workflow.Review)
			if !done.Review.Accepted || len(done.ReviewHistory) != 1 || canceled.stops != 0 {
				t.Fatalf("cancellation duplicated review: %+v", done)
			}
		})
	}
}
