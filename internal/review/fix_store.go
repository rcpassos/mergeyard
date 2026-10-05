package review

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

type FixSnapshot struct {
	Agent          string     `json:"agent"`
	SessionID      string     `json:"session_id"`
	Model          string     `json:"model"`
	Effort         string     `json:"effort"`
	Skills         []string   `json:"skills"`
	PermissionMode string     `json:"permission_mode"`
	AllowedTools   []string   `json:"allowed_tools"`
	Round          int        `json:"round"`
	Attempt        int        `json:"attempt"`
	TargetSHA      string     `json:"target_sha"`
	CommitSHA      string     `json:"commit_sha,omitempty"`
	Pushed         bool       `json:"pushed"`
	Status         string     `json:"attempt_status"`
	Findings       []Finding  `json:"findings"`
	Report         *FixReport `json:"report,omitempty"`
	Error          string     `json:"error,omitempty"`
}

const fixSnapshotQuery = `SELECT a.agent,f.session_id,COALESCE(a.model,''),COALESCE(a.effort,''),COALESCE(a.skills_json,'[]'),f.permission_mode,f.allowed_tools_json,a.round,a.attempt,f.target_sha,COALESCE(f.commit_sha,''),f.pushed,a.status,f.findings_json,f.report_json,COALESCE(a.error,'') FROM fix_attempts f JOIN phase_attempts a ON a.id=f.attempt_id WHERE a.run_id=?`

func scanFix(row interface{ Scan(...any) error }) (*FixSnapshot, error) {
	var v FixSnapshot
	var skills, tools, findings string
	var report sql.NullString
	err := row.Scan(&v.Agent, &v.SessionID, &v.Model, &v.Effort, &skills, &v.PermissionMode, &tools, &v.Round, &v.Attempt, &v.TargetSHA, &v.CommitSHA, &v.Pushed, &v.Status, &findings, &report, &v.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, item := range []struct {
		raw    string
		target any
	}{{skills, &v.Skills}, {tools, &v.AllowedTools}, {findings, &v.Findings}} {
		if err := json.Unmarshal([]byte(item.raw), item.target); err != nil {
			return nil, err
		}
	}
	if report.Valid {
		v.Report = &FixReport{}
		if err := json.Unmarshal([]byte(report.String), v.Report); err != nil {
			return nil, err
		}
	}
	return &v, nil
}

// LoadFixHistory keeps prior findings and responses visible after later rounds.
func LoadFixHistory(ctx context.Context, db queryer, id string) ([]FixSnapshot, error) {
	rows, err := db.QueryContext(ctx, fixSnapshotQuery+" ORDER BY a.round,a.attempt", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var history []FixSnapshot
	for rows.Next() {
		v, err := scanFix(rows)
		if err != nil {
			return nil, err
		}
		history = append(history, *v)
	}
	return history, rows.Err()
}
