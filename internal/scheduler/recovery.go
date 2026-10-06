package scheduler

import (
	"context"
	"database/sql"
	"errors"

	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func missingConversation(err error) bool {
	var coded *fault.Error
	return errors.As(err, &coded) && coded.Code == "harness.session_resume_failed"
}

func phaseRound(run workflow.Run, phase workflow.Phase) int {
	if phase == workflow.Implement {
		return 0
	}
	return run.ReviewRound
}

// A physical attempt number identifies artifacts; only ordinary executions spend
// the configured budget. One missing startup per phase/round can be replaced.
func (s *Scheduler) retryPhase(ctx context.Context, run workflow.Run, phase workflow.Phase, a attempt, cause error, max int) (bool, error) {
	var recovered int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM session_recoveries WHERE run_id=? AND phase=? AND round=?", run.ID, phase, phaseRound(run, phase)).Scan(&recovered)
	if err != nil {
		return false, err
	}
	if missingConversation(cause) {
		return a.resumed && recovered == 0, nil
	}
	if errorCodeForReview(cause) == "review.head_changed" {
		return false, nil
	}
	// Explicit retry opens a new configured attempt window, retaining recovery.
	floor := 1
	for _, v := range run.Retries {
		if !v.Pending && v.Error == "" && v.NextPhase == phase && v.Round == run.ReviewRound && v.AttemptFrom > floor {
			floor = v.AttemptFrom
		}
	}
	if floor > 1 {
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM session_recoveries r JOIN phase_attempts a ON a.id=r.replacement_attempt_id WHERE r.run_id=? AND r.phase=? AND r.round=? AND a.attempt>=?", run.ID, phase, phaseRound(run, phase), floor).Scan(&recovered); err != nil {
			return false, err
		}
	}
	number := a.number - floor + 1 - recovered
	if phase == workflow.Implement {
		return canRetryImplementAttempt(cause, number, max), nil
	}
	return canRetryAttempt(cause, number, max), nil
}

// reserveRecovery is part of the replacement's launch transaction. A crash after
// commit leaves a running deterministic identity for observation, never replay.
func reserveRecovery(ctx context.Context, tx *sql.Tx, run workflow.Run, phase workflow.Phase, previous attempt, replacement, session string) (events.Draft, error) {
	role := "implementer"
	if phase == workflow.Review {
		role = "reviewer"
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO session_recoveries(run_id,phase,role,round,failed_attempt_id,replacement_attempt_id,previous_session_id) VALUES (?,?,?,?,?,?,?)`, run.ID, phase, role, phaseRound(run, phase), previous.id, replacement, previous.sessionID)
	return events.Draft{RunID: run.ID, Type: "harness.session_resume_failed", Payload: map[string]any{"agent": previous.agent, "phase": phase, "role": role, "round": phaseRound(run, phase), "attempt": previous.number, "replacement_attempt": previous.number + 1, "previous_session_id": previous.sessionID, "session_id": session, "error": previous.failure, "warning": "Saved conversation is missing; continuing once in a fresh session"}}, err
}
