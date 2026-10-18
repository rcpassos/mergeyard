package review

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/rcpassos/mergeyard/internal/contract"
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
		// Versions without prior findings, including v1, display an empty list.
		if requirement, ok := contract.For(contract.Review, v.Report.SchemaVersion); ok && !requirement.Requires(contract.PriorFindings) {
			v.Report.PriorFindings = []PriorFinding{}
		}
	}
	return &v, nil
}

// LoadPriorContext uses the latest accepted review at or before this round.
// A changed-head Retry can replace an accepted review within the same round;
// failed attempts must not erase that accepted report or its fix responses.
func LoadPriorContext(ctx context.Context, db queryer, runID string, round int) ([]Finding, *FixReport, error) {
	findings := []Finding{}
	if round < 1 {
		return findings, nil, nil
	}
	var raw string
	var priorRound int
	var priorOrder int64
	var priorTarget string
	err := db.QueryRowContext(ctx, `SELECT v.report_json,a.round,a.rowid,v.target_sha FROM review_attempts v JOIN phase_attempts a ON a.id=v.attempt_id WHERE a.run_id=? AND a.round<=? AND v.accepted=1 ORDER BY a.round DESC,a.attempt DESC LIMIT 1`, runID, round).Scan(&raw, &priorRound, &priorOrder, &priorTarget)
	if errors.Is(err, sql.ErrNoRows) {
		return findings, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var report Report
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		return nil, nil, err
	}
	findings = report.Findings
	// A blocked/failed fix can still contain validated partial responses. Those
	// disputes also need a ruling when manual handback advances the review.
	// Fixes must follow the selected review and address its pinned target. Earlier
	// fixes have already been adjudicated by that review, even on the same SHA.
	// SQLite rowid supplies insertion order without timestamp precision ties.
	err = db.QueryRowContext(ctx, `SELECT f.report_json FROM fix_attempts f JOIN phase_attempts a ON a.id=f.attempt_id WHERE a.run_id=? AND a.round=? AND a.rowid>? AND f.target_sha=? AND f.report_json IS NOT NULL ORDER BY a.attempt DESC LIMIT 1`, runID, priorRound, priorOrder, priorTarget).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return findings, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var fix FixReport
	if err := json.Unmarshal([]byte(raw), &fix); err != nil {
		return nil, nil, err
	}
	return findings, &fix, nil
}
