package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/rcpassos/mergeyard/internal/acceptance"
	"github.com/rcpassos/mergeyard/internal/contract"
	"github.com/rcpassos/mergeyard/internal/verification"
)

// ImplementReport retains the implementer's evidence for display.
type ImplementReport struct {
	Verification  []verification.Entry  `json:"verification"`
	SchemaVersion int                   `json:"schema_version"`
	Status        string                `json:"status"`
	Summary       string                `json:"summary"`
	Criteria      []acceptance.Evidence `json:"criteria"`
}

// HasCriteria reports whether the saved contract records acceptance criteria.
func (r ImplementReport) HasCriteria() bool {
	requirement, ok := contract.For(contract.Implement, r.SchemaVersion)
	return ok && requirement.Requires(contract.Criteria)
}

// HasVerification reports whether the saved contract records verification.
func (r ImplementReport) HasVerification() bool {
	requirement, ok := contract.For(contract.Implement, r.SchemaVersion)
	return ok && requirement.Requires(contract.Verification)
}

// ImplementSnapshot is the last durable implement attempt, retained during review.
type ImplementSnapshot struct {
	Report         *ImplementReport `json:"report,omitempty"`
	ReportEligible bool             `json:"report_eligible"`
	// Sequence orders evidence exactly like publication; it is not display data.
	Sequence       int64    `json:"-"`
	CheckRetryOf   string   `json:"check_retry_of,omitempty"`
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

const implementSnapshotQuery = `SELECT a.agent,COALESCE(r.implementer_session_id,''),a.session_id,COALESCE(a.model,''),COALESCE(a.effort,''),COALESCE(a.skills_json,'[]'),COALESCE(a.permissions,'Not recorded for this older attempt'),a.attempt,a.status,COALESCE(a.process_session,''),COALESCE(a.error,''),a.check_retry_of,a.report_json,a.report_eligible,a.rowid
 FROM phase_attempts a JOIN runs r ON r.id=a.run_id WHERE a.run_id=? AND a.phase='implement' AND a.step=''`

// LoadImplementSnapshot reads the latest attempt for active-session controls.
func LoadImplementSnapshot(ctx context.Context, db queryer, id string) (*ImplementSnapshot, error) {
	record, err := scanImplement(db.QueryRowContext(ctx, implementSnapshotQuery+" ORDER BY a.attempt DESC LIMIT 1", id))
	if record == nil || err != nil {
		return nil, err
	}
	return &record.snapshot, nil
}

// LoadImplementHistory retains each agent attempt and its evidence across retries.
func LoadImplementHistory(ctx context.Context, db queryer, id string) ([]ImplementSnapshot, error) {
	rows, err := db.QueryContext(ctx, implementSnapshotQuery+" ORDER BY a.attempt", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var history []ImplementSnapshot
	for rows.Next() {
		record, err := scanImplement(rows)
		if err != nil {
			return nil, err
		}
		// History identifies the recorded conversation for this attempt, rather
		// than borrowing the active conversation used by the current controls.
		record.snapshot.SessionID = record.sessionID.String
		history = append(history, record.snapshot)
	}
	return history, rows.Err()
}

type implementRecord struct {
	snapshot  ImplementSnapshot
	sessionID sql.NullString
}

func scanImplement(row interface{ Scan(...any) error }) (*implementRecord, error) {
	var record implementRecord
	v := &record.snapshot
	var skills string
	var raw sql.NullString
	err := row.Scan(&v.Agent, &v.SessionID, &record.sessionID, &v.Model, &v.Effort, &skills, &v.Permissions, &v.Attempt, &v.Status, &v.ProcessSession, &v.Error, &v.CheckRetryOf, &raw, &v.ReportEligible, &v.Sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(skills), &v.Skills); err != nil {
		return nil, err
	}
	// The attempt owns its report; migration 029 backfilled attempts written by
	// older binaries from their completion events.
	if raw.Valid {
		var report ImplementReport
		if err := json.Unmarshal([]byte(raw.String), &report); err != nil {
			return nil, err
		}
		if report.SchemaVersion > 0 {
			v.Report = &report
		}
	}
	return &record, nil
}
