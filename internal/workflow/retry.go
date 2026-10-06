package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
)

// RetrySnapshot records human intent and its reconciled destination. AttemptFrom
// opens a configured attempt window without changing physical attempt identities.
// GrantedRound is an absolute ceiling, so replay cannot add another round.
type RetrySnapshot struct {
	ID           int64  `json:"id"`
	Pending      bool   `json:"pending"`
	RequestedAt  string `json:"requested_at"`
	Cause        string `json:"cause"`
	NextState    State  `json:"next_state,omitempty"`
	NextPhase    Phase  `json:"next_phase,omitempty"`
	Round        int    `json:"round"`
	AttemptFrom  int    `json:"attempt_from,omitempty"`
	GrantedRound int    `json:"granted_round,omitempty"`
	TargetSHA    string `json:"target_sha,omitempty"`
	Deadline     string `json:"deadline,omitempty"`
	Error        string `json:"error,omitempty"`
}

func LoadRetries(ctx context.Context, db queryer, id string) ([]RetrySnapshot, error) {
	rows, err := db.QueryContext(ctx, "SELECT id,pending,snapshot_json FROM run_retries WHERE run_id=? ORDER BY id", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var history []RetrySnapshot
	for rows.Next() {
		var v RetrySnapshot
		var data string
		var id int64
		var pending bool
		if err := rows.Scan(&id, &pending, &data); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			return nil, err
		}
		v.ID, v.Pending = id, pending
		history = append(history, v)
	}
	return history, rows.Err()
}

func (v RetrySnapshot) Save(ctx context.Context, tx *sql.Tx, runID string) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "UPDATE run_retries SET pending=?,snapshot_json=? WHERE id=? AND run_id=? AND pending=1", v.Pending, string(data), v.ID, runID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return invalid("Retry intent is no longer pending or belongs to another run")
	}
	return nil
}

func (r Run) PendingRetry() *RetrySnapshot {
	if len(r.Retries) > 0 && r.Retries[len(r.Retries)-1].Pending {
		v := r.Retries[len(r.Retries)-1]
		return &v
	}
	return nil
}
