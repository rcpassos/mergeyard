package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// ImplementSnapshot is the last durable implement attempt, retained during review.
type ImplementSnapshot struct {
	Agent          string   `json:"agent"`
	SessionID      string   `json:"session_id"`
	Model          string   `json:"model"`
	Effort         string   `json:"effort"`
	Skills         []string `json:"skills"`
	Permissions    string   `json:"permissions"`
	Attempt        int      `json:"attempt"`
	Status         string   `json:"status"`
	ProcessSession string   `json:"process_session"`
	Error          string   `json:"error,omitempty"`
}

// LoadImplementSnapshot reads persisted attempt settings without using current config.
func LoadImplementSnapshot(ctx context.Context, db queryer, id string) (*ImplementSnapshot, error) {
	var v ImplementSnapshot
	var skills string
	err := db.QueryRowContext(ctx, `SELECT a.agent,COALESCE(r.implementer_session_id,''),COALESCE(a.model,''),COALESCE(a.effort,''),COALESCE(a.skills_json,'[]'),COALESCE(a.permissions,'Not recorded for this older attempt'),a.attempt,a.status,COALESCE(a.process_session,''),COALESCE(a.error,'') FROM phase_attempts a JOIN runs r ON r.id=a.run_id WHERE a.run_id=? AND a.phase='implement' ORDER BY a.attempt DESC LIMIT 1`, id).Scan(&v.Agent, &v.SessionID, &v.Model, &v.Effort, &skills, &v.Permissions, &v.Attempt, &v.Status, &v.ProcessSession, &v.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(skills), &v.Skills); err != nil {
		return nil, err
	}
	return &v, nil
}
