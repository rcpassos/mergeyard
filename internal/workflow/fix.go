package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/rcpassos/mergeyard/internal/review"
)

// FixCompletion verifies the durable publication journal in the same transaction
// that clears approval and advances exactly one review round.
type FixCompletion struct{ AttemptID string }

func (f FixCompletion) apply(ctx context.Context, tx *sql.Tx, id string, patch MetadataPatch) error {
	if !patch.IncrementReviewRound || patch.ApprovedSHA == nil || *patch.ApprovedSHA != "" {
		return invalid("Fix completion must clear approval and advance the review round")
	}
	var reportJSON string
	err := tx.QueryRowContext(ctx, `SELECT f.report_json FROM fix_attempts f JOIN phase_attempts a ON a.id=f.attempt_id WHERE a.id=? AND a.run_id=? AND a.phase='fix' AND a.status='succeeded' AND a.round=(SELECT review_round FROM runs WHERE id=a.run_id) AND f.pushed=1 AND f.commit_sha IS NOT NULL`, f.AttemptID, id).Scan(&reportJSON)
	if err != nil {
		return invalid("Fix must have a completed attempt and pinned, published commit")
	}
	var report review.FixReport
	if err := json.Unmarshal([]byte(reportJSON), &report); err != nil {
		return invalid("Invalid fix report")
	}
	if err := report.Validate(); err != nil || report.Status != "success" {
		return invalid("Fix completion requires a valid success report")
	}
	return nil
}
