package scheduler

import (
	"context"
	"errors"

	"github.com/rcpassos/mergeyard/internal/execution/durable"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func missingConversation(err error) bool {
	var coded *fault.Error
	return errors.As(err, &coded) && coded.Code == "harness.session_resume_failed"
}

// A physical attempt number identifies artifacts; only ordinary executions spend
// the configured budget. One missing startup per phase/round can be replaced.
func (s *Scheduler) retryPhase(ctx context.Context, run workflow.Run, phase workflow.Phase, a durable.Attempt, cause error, max int) (bool, error) {
	round := durable.AttemptRound(run, phase)
	var recovered int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM session_recoveries WHERE run_id=? AND phase=? AND round=?", run.ID, phase, round).Scan(&recovered)
	if err != nil {
		return false, err
	}
	if missingConversation(cause) {
		return a.Resumed && recovered == 0, nil
	}
	if errorCodeForReview(cause) == "review.head_changed" {
		return false, nil
	}
	if a.CheckRetryOf != "" {
		return false, nil
	}
	// Human operations open a new attempt window without replenishing recovery.
	floor := attemptWindowStart(run, phase)
	if floor > 1 {
		// Discount the missing startup only when it belonged to this window.
		// Its replacement may be the first ordinary execution after handback.
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM session_recoveries r JOIN phase_attempts a ON a.id=r.failed_attempt_id WHERE r.run_id=? AND r.phase=? AND r.round=? AND a.attempt>=?", run.ID, phase, round, floor).Scan(&recovered); err != nil {
			return false, err
		}
	}
	var ordinary int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM phase_attempts WHERE run_id=? AND phase=? AND round=? AND attempt>=? AND step='' AND check_retry_of='' AND status!='usage_limited'", run.ID, phase, round, floor).Scan(&ordinary); err != nil {
		return false, err
	}
	number := ordinary - recovered
	return canRetryAttempt(cause, number, max), nil
}
