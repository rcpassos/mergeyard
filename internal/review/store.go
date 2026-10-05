package review

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

const snapshotQuery = `SELECT a.agent,v.session_id,COALESCE(a.model,''),COALESCE(a.effort,''),COALESCE(a.skills_json,'[]'),v.permission_mode,v.allowed_tools_json,a.round,a.attempt,v.target_sha,a.status,v.restored,v.contaminated,v.accepted,v.report_json,COALESCE(a.error,'') FROM review_attempts v JOIN phase_attempts a ON a.id=v.attempt_id WHERE a.run_id=?`

// LoadSnapshot reads the latest durable reviewer attempt for display.
func LoadSnapshot(ctx context.Context, db queryer, id string) (*Snapshot, error) {
	return scanReview(db.QueryRowContext(ctx, snapshotQuery+" ORDER BY a.round DESC,a.attempt DESC LIMIT 1", id))
}

// LoadHistory keeps every review visible after retries and later rounds.
func LoadHistory(ctx context.Context, db queryer, id string) ([]Snapshot, error) {
	rows, err := db.QueryContext(ctx, snapshotQuery+" ORDER BY a.round,a.attempt", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Snapshot
	for rows.Next() {
		v, err := scanReview(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *v)
	}
	return result, rows.Err()
}

func scanReview(row interface{ Scan(...any) error }) (*Snapshot, error) {
	var v Snapshot
	var skills, tools string
	var report sql.NullString
	err := row.Scan(&v.Agent, &v.SessionID, &v.Model, &v.Effort, &skills, &v.PermissionMode, &tools, &v.Round, &v.Attempt, &v.TargetSHA, &v.Status, &v.Restored, &v.Contaminated, &v.Accepted, &report, &v.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(skills), &v.Skills); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(tools), &v.AllowedTools); err != nil {
		return nil, err
	}
	if report.Valid {
		v.Report = &Report{}
		if err := json.Unmarshal([]byte(report.String), v.Report); err != nil {
			return nil, err
		}
	}
	return &v, nil
}
