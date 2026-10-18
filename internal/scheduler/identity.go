package scheduler

import (
	"context"
	"database/sql"
	"errors"

	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// Observe the existing capture, including after an offline finish. No new
// process or session is created to recover identity after a control-plane restart.
func (s *Scheduler) discoverImplementSession(ctx context.Context, id string, a *attempt) error {
	return s.discoverSession(ctx, id, workflow.Implement, a)
}

func (s *Scheduler) discoverSession(ctx context.Context, id string, phase workflow.Phase, a *attempt) error {
	saved := durableAttempt(*a)
	err := s.execution().Discover(ctx, id, phase, &saved)
	*a = schedulerAttempt(saved)
	return err
}

// Identity observation is local and does not wait for the GitHub polling interval.
// It shares the tick and per-run gates with reconciliation and lifecycle controls.
func (s *Scheduler) observeIdentities(ctx context.Context) error {
	s.tick.Lock()
	defer s.tick.Unlock()
	// Only a run waiting on an unidentified attempt can discover an identity,
	// so the pass reads candidates instead of reloading the whole history.
	candidates, err := s.execution().Discoverable(ctx)
	if err != nil {
		return err
	}
	for _, runID := range candidates {
		err := s.workflow.WithRunOperation(ctx, runID, func(ctx context.Context, current workflow.Run) error {
			if current.State != workflow.Active {
				return nil
			}
			var a attempt
			var err error
			switch current.Phase {
			case workflow.Implement:
				a, err = s.lastAttempt(ctx, current.ID)
			case workflow.Review:
				var review reviewAttempt
				review, err = s.lastReview(ctx, current)
				a = review.attempt
			case workflow.Fix:
				var fix fixAttempt
				fix, err = s.lastFix(ctx, current)
				a = fix.attempt
			default:
				return nil
			}
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if a.status != "running" || a.sessionID != "" {
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

func invalidSessionDiscovery(err error) bool {
	var coded *fault.Error
	return errors.As(err, &coded) && (coded.Code == "phase.result_invalid" || coded.Code == "harness.session_identity_invalid" || coded.Code == "harness.session_resume_failed")
}
