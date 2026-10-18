package durable

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// FinishImplement journals the interpreted report, attempt status and process
// exit atomically. The caller has already applied limit classification and
// task-result semantics. The attempt's report_json is the durable evidence
// readers use; the completion event records the phase, attempt, result and
// error as the run's history.
func (e Executor) FinishImplement(ctx context.Context, runID string, a Attempt, exitCode *int, result harness.PhaseResult, err error) error {
	attemptStatus, event := "succeeded", "phase.completed"
	if err != nil {
		attemptStatus, event = "failed", "phase.failed"
	}
	failureText := ""
	if err != nil {
		failureText = err.Error()
	}
	var report any
	eligible := 0
	// A zero schema version means result parsing never produced a report. The
	// eligibility answer is frozen here because later events may reject the
	// attempt without withdrawing its already-saved report.
	if result.SchemaVersion > 0 {
		encoded, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			return marshalErr
		}
		report = string(encoded)
		if workflow.ValidCompletion(err) {
			eligible = 1
		}
	}
	_, saveErr := e.Events.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		_, saveErr := tx.ExecContext(ctx, `UPDATE phase_attempts SET status=?,exit_code=?,ended_at=strftime('%Y-%m-%dT%H:%M:%fZ','now'),error=?,report_json=COALESCE(?,report_json),report_eligible=MAX(report_eligible,?) WHERE id=?`, attemptStatus, exitCode, failureText, report, eligible, a.ID)
		return events.Draft{RunID: runID, Type: event, Payload: map[string]any{"phase": workflow.Implement, "attempt": a.Number, "result": result, "error": failureText}}, saveErr
	})
	if saveErr != nil {
		return saveErr
	}
	return nil
}
