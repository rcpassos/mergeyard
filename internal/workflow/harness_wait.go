package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/rcpassos/mergeyard/internal/events"
	"time"
)

// HarnessWait retains every interrupted execution and its reset provenance.
// Empty AttemptID denotes account gating before a new execution, not a limit
// observation. Consecutive and Sequence identify a bounded phase/round streak.
type HarnessWait struct {
	Harness         string    `json:"harness"`
	Reason          string    `json:"reason"`
	Phase           Phase     `json:"phase"`
	Round           int       `json:"round"`
	AttemptID       string    `json:"attempt_id,omitempty"`
	Attempt         int       `json:"attempt,omitempty"`
	ExitCode        *int      `json:"exit_code,omitempty"`
	DetectedAt      time.Time `json:"detected_at"`
	ResetAt         time.Time `json:"reset_at"`
	ResetTimeSource string    `json:"reset_time_source"`
	Source          string    `json:"source"`
	Consecutive     int       `json:"consecutive"`
	Sequence        int       `json:"sequence"`
	Allowance       int       `json:"allowance"`
}

// Role follows the saved phase, including account gates without an execution.
func (v HarnessWait) Role() string {
	if v.Phase == Review {
		return "reviewer"
	}
	return "implementer"
}

func LoadHarnessWaits(ctx context.Context, db queryer, id string) ([]HarnessWait, error) {
	rows, err := db.QueryContext(ctx, "SELECT snapshot_json FROM harness_waits WHERE run_id=? ORDER BY id", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var history []HarnessWait
	for rows.Next() {
		var data string
		var v HarnessWait
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			return nil, err
		}
		history = append(history, v)
	}
	return history, rows.Err()
}

func (v HarnessWait) save(ctx context.Context, tx *sql.Tx, id string) (*events.Draft, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var attempt any
	if v.AttemptID != "" {
		attempt = v.AttemptID
		result, err := tx.ExecContext(ctx, `UPDATE phase_attempts SET status='usage_limited',exit_code=?,ended_at=strftime('%Y-%m-%dT%H:%M:%fZ','now'),error='harness.temporary_limit: Execution interrupted by a temporary usage limit' WHERE id=? AND run_id=? AND phase=? AND round=? AND status='running'`, v.ExitCode, v.AttemptID, id, v.Phase, v.Round)
		if err != nil {
			return nil, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n != 1 {
			return nil, invalid("Usage limit requires the current running execution")
		}
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO harness_waits(run_id,phase_attempt_id,snapshot_json) VALUES(?,?,?)", id, attempt, string(data))
	if err != nil {
		return nil, err
	}
	if v.AttemptID != "" {
		return saveHarnessLimit(ctx, tx, v)
	}
	return nil, nil
}
