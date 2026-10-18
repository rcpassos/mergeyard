package scheduler

import (
	"context"
	"database/sql"
	"errors"

	"github.com/rcpassos/mergeyard/internal/execution/durable"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// Observe the existing capture, including after an offline finish. No new
// process or session is created to recover identity after a control-plane restart.
func (s *Scheduler) discoverImplementSession(ctx context.Context, id string, a *durable.Attempt) error {
	return s.discoverSession(ctx, id, workflow.Implement, a)
}

func (s *Scheduler) discoverSession(ctx context.Context, id string, phase workflow.Phase, a *durable.Attempt) error {
	return s.execution().Discover(ctx, id, phase, a)
}

// Identity observation is local and does not wait for the GitHub polling interval.
// It shares the tick and per-run gates with reconciliation and lifecycle controls.
func (s *Scheduler) observeIdentities(ctx context.Context) error {
	s.tick.Lock()
	defer s.tick.Unlock()
	runs, err := s.Runs(ctx)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.State != workflow.Active {
			continue
		}
		err := s.workflow.WithRunOperation(ctx, run.ID, func(ctx context.Context, current workflow.Run) error {
			if current.State != workflow.Active {
				return nil
			}
			var a durable.Attempt
			var err error
			switch current.Phase {
			case workflow.Implement:
				a, err = s.lastAttempt(ctx, current.ID)
			case workflow.Review:
				var review reviewAttempt
				review, err = s.lastReview(ctx, current)
				a = review.Attempt
			case workflow.Fix:
				var fix fixAttempt
				fix, err = s.lastFix(ctx, current)
				a = fix.Attempt
			default:
				return nil
			}
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if a.Status != "running" || a.SessionID != "" {
				return nil
			}
			return s.discoverSession(ctx, current.ID, current.Phase, &a)
		})
		if err != nil {
			return err
		}
	}
	return nil
}
