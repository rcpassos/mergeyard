package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/rcpassos/mergeyard/internal/acceptance"
)

// ImplementReport retains the implementer's evidence for display.
type ImplementReport struct {
	SchemaVersion int                   `json:"schema_version"`
	Status        string                `json:"status"`
	Summary       string                `json:"summary"`
	Criteria      []acceptance.Evidence `json:"criteria"`
}

// ImplementSnapshot is the last durable implement attempt, retained during review.
type ImplementSnapshot struct {
	Report         *ImplementReport `json:"report,omitempty"`
	CheckRetryOf   string           `json:"check_retry_of,omitempty"`
	Agent          string           `json:"agent"`
	SessionID      string           `json:"session_id"`
	Model          string           `json:"model"`
	Effort         string           `json:"effort"`
	Skills         []string         `json:"skills"`
	Permissions    string           `json:"permissions"`
	Attempt        int              `json:"attempt"`
	Status         string           `json:"status"`
	ProcessSession string           `json:"process_session"`
	Error          string           `json:"error,omitempty"`
}

// LoadImplementSnapshot reads persisted attempt settings without using current config.
func LoadImplementSnapshot(ctx context.Context, db queryer, id string) (*ImplementSnapshot, error) {
	var v ImplementSnapshot
	var skills string
	err := db.QueryRowContext(ctx, `SELECT a.agent,COALESCE(r.implementer_session_id,''),COALESCE(a.model,''),COALESCE(a.effort,''),COALESCE(a.skills_json,'[]'),COALESCE(a.permissions,'Not recorded for this older attempt'),a.attempt,a.status,COALESCE(a.process_session,''),COALESCE(a.error,''),a.check_retry_of FROM phase_attempts a JOIN runs r ON r.id=a.run_id WHERE a.run_id=? AND a.phase='implement' AND a.step='' ORDER BY a.attempt DESC LIMIT 1`, id).Scan(&v.Agent, &v.SessionID, &v.Model, &v.Effort, &skills, &v.Permissions, &v.Attempt, &v.Status, &v.ProcessSession, &v.Error, &v.CheckRetryOf)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(skills), &v.Skills); err != nil {
		return nil, err
	}
	// Completion events store the validated report in the same transaction as
	// attempt status. Explicit phase filtering keeps later fix/review events out;
	// older implement events omit phase, so they retain their original meaning.
	var raw string
	err = db.QueryRowContext(ctx, `SELECT payload_json FROM events WHERE run_id=? AND type IN ('phase.completed','phase.failed') AND COALESCE(json_extract(payload_json,'$.phase'),'implement')='implement' AND json_extract(payload_json,'$.attempt')=? ORDER BY id DESC LIMIT 1`, id, v.Attempt).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return &v, nil
	}
	if err != nil {
		return nil, err
	}
	var completed struct {
		Report *ImplementReport `json:"result"`
	}
	if err := json.Unmarshal([]byte(raw), &completed); err != nil {
		return nil, err
	}
	if completed.Report != nil && completed.Report.SchemaVersion > 0 {
		v.Report = completed.Report
	}
	return &v, nil
}
