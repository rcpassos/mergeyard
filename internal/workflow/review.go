package workflow

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/rcpassos/mergeyard/internal/review"
)

// ReviewCompletion commits a validated, restored attempt alongside its verdict
// transition and approved SHA. The workflow validates ownership and target.
type ReviewCompletion struct {
	AttemptID string
	Report    review.Report
	ExitCode  int
}

func (r ReviewCompletion) apply(ctx context.Context, tx *sql.Tx, id string, patch MetadataPatch) error {
	if err := r.Report.Validate(); err != nil {
		return invalid(err.Error())
	}
	var target string
	var restored, contaminated bool
	err := tx.QueryRowContext(ctx, `SELECT v.target_sha,v.restored,v.contaminated FROM review_attempts v JOIN phase_attempts a ON a.id=v.attempt_id WHERE a.id=? AND a.run_id=? AND a.phase='review' AND a.status='running' AND a.round=(SELECT review_round FROM runs WHERE id=a.run_id)`, r.AttemptID, id).Scan(&target, &restored, &contaminated)
	if err != nil {
		return invalid("Review attempt does not belong to this running review")
	}
	if !restored || contaminated {
		return invalid("Review must be restored and uncontaminated before accepting a verdict")
	}
	if r.Report.Status == "approved" && (patch.ApprovedSHA == nil || *patch.ApprovedSHA != target) {
		return invalid("Approval must match the pinned review target")
	}
	data, err := json.Marshal(r.Report)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE review_attempts SET report_json=?,accepted=1 WHERE attempt_id=?", string(data), r.AttemptID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE phase_attempts SET status='succeeded',exit_code=?,ended_at=strftime('%Y-%m-%dT%H:%M:%fZ','now'),error=NULL WHERE id=?`, r.ExitCode, r.AttemptID)
	return err
}
