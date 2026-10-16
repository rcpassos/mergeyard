package durable

import (
	"context"
	"database/sql"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/harness"
)

// FinishImplement journals the interpreted result and process exit atomically.
// The caller has already applied limit classification and task-result semantics.
func (e Executor) FinishImplement(ctx context.Context, runID string, a Attempt, exitCode *int, result harness.PhaseResult, err error) error {
	attemptStatus, event := "succeeded", "phase.completed"
	if err != nil {
		attemptStatus, event = "failed", "phase.failed"
	}
	failureText := ""
	if err != nil {
		failureText = err.Error()
	}
	_, saveErr := e.Events.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		_, saveErr := tx.ExecContext(ctx, `UPDATE phase_attempts SET status=?,exit_code=?,ended_at=strftime('%Y-%m-%dT%H:%M:%fZ','now'),error=? WHERE id=?`, attemptStatus, exitCode, failureText, a.ID)
		return events.Draft{RunID: runID, Type: event, Payload: map[string]any{"attempt": a.Number, "result": result, "error": failureText}}, saveErr
	})
	if saveErr != nil {
		return saveErr
	}
	return nil
}
