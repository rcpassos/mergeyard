package workflow

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
)

// HarnessCheck owns one minimal execution, independent of any engineering run.
type HarnessCheck struct {
	ID             string `json:"id"`
	Harness        string `json:"harness"`
	RestrictionID  string `json:"restriction_id"`
	RequestJSON    string `json:"-"`
	ProcessSession string `json:"-"`
	PhaseDir       string `json:"-"`
	Status         string `json:"status"`
	Result         string `json:"result,omitempty"`
	RequestedAt    string `json:"requested_at"`
	EndedAt        string `json:"ended_at,omitempty"`
}

func LoadHarnessChecks(ctx context.Context, db queryer, harness string, active bool) ([]HarnessCheck, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,harness,restriction_id,request_json,process_session,phase_dir,status,result,requested_at,COALESCE(ended_at,'') FROM harness_checks WHERE (?='' OR harness=?) AND (?=0 OR status IN ('reserved','running')) ORDER BY requested_at,rowid`, harness, harness, active)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var checks []HarnessCheck
	for rows.Next() {
		var c HarnessCheck
		if err := rows.Scan(&c.ID, &c.Harness, &c.RestrictionID, &c.RequestJSON, &c.ProcessSession, &c.PhaseDir, &c.Status, &c.Result, &c.RequestedAt, &c.EndedAt); err != nil {
			return nil, err
		}
		checks = append(checks, c)
	}
	return checks, rows.Err()
}

func (w *Workflow) ReserveHarnessCheck(ctx context.Context, c HarnessCheck, now time.Time) error {
	_, err := w.events.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		limit, err := LoadHarnessLimit(ctx, tx, c.Harness)
		if err != nil {
			return events.Draft{}, err
		}
		if limit == nil || limit.Reason != "credits_exhausted" || limit.RestrictionID != c.RestrictionID || limit.ProbeID != "" {
			return events.Draft{}, &fault.Error{Code: "harness.probe_unavailable", Message: "Check availability requires an exhausted-credit block with no other recovery probe; refresh status"}
		}
		if now.Before(limit.ResetAt) {
			return events.Draft{}, &fault.Error{Code: "harness.check_waiting", Message: "Wait for the temporary usage-limit reset before checking availability"}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO harness_checks(id,harness,restriction_id,request_json,process_session,phase_dir,status) VALUES(?,?,?,?,?,?,'reserved')`, c.ID, c.Harness, c.RestrictionID, c.RequestJSON, c.ProcessSession, c.PhaseDir); err != nil {
			return events.Draft{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO credit_probes(id,harness,restriction_id,check_id,status) VALUES(?,?,?,?,'reserved')`, c.ID, c.Harness, c.RestrictionID, c.ID); err != nil {
			return events.Draft{}, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE harness_limits SET probe_id=? WHERE harness_type=?`, c.ID, c.Harness)
		return events.Draft{Type: "harness.probe_reserved", Payload: c}, err
	})
	return err
}

func (w *Workflow) StartHarnessCheck(ctx context.Context, c HarnessCheck, now time.Time) error {
	_, err := w.events.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		limit, err := LoadHarnessLimit(ctx, tx, c.Harness)
		if err != nil {
			return events.Draft{}, err
		}
		if limit == nil || limit.ProbeID != c.ID || limit.RestrictionID != c.RestrictionID || now.Before(limit.ResetAt) {
			return events.Draft{}, &fault.Error{Code: "harness.probe_unavailable", Message: "Restriction changed before the availability request; check status again"}
		}
		updated, err := tx.ExecContext(ctx, `UPDATE credit_probes SET status='running' WHERE id=? AND status='reserved'`, c.ID)
		if err != nil {
			return events.Draft{}, err
		}
		count, err := updated.RowsAffected()
		if err != nil {
			return events.Draft{}, err
		}
		if count != 1 {
			return events.Draft{}, &fault.Error{Code: "harness.probe_unavailable", Message: "This availability execution has already been claimed; refresh status"}
		}
		_, err = tx.ExecContext(ctx, `UPDATE harness_checks SET status='running',request_json=? WHERE id=? AND status='reserved'`, c.RequestJSON, c.ID)
		c.Status = "running"
		return events.Draft{Type: "harness.check_started", Payload: c}, err
	})
	return err
}

// CompleteHarnessCheck records the result and proof atomically. Further limit
// observations are applied only while this check still owns its generation.
func (w *Workflow) CompleteHarnessCheck(ctx context.Context, c HarnessCheck, now time.Time, proven bool, result string, wait *HarnessWait) error {
	_, err := w.events.CommitBatch(ctx, func(tx *sql.Tx) ([]events.Draft, error) {
		var p CreditProbe
		err := tx.QueryRowContext(ctx, `SELECT p.id,p.harness,p.restriction_id,p.status FROM credit_probes p JOIN harness_checks c ON c.id=p.check_id WHERE p.check_id=? AND c.status IN ('reserved','running')`, c.ID).Scan(&p.ID, &p.Harness, &p.RestrictionID, &p.Status)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errHarnessUnchanged
		}
		if err != nil {
			return nil, err
		}
		limit, err := LoadHarnessLimit(ctx, tx, c.Harness)
		if err != nil {
			return nil, err
		}
		var drafts []events.Draft
		if wait != nil && limit != nil && limit.ProbeID == p.ID && limit.RestrictionID == p.RestrictionID {
			if wait.Reason == "credits_exhausted" {
				_, err = tx.ExecContext(ctx, `UPDATE harness_limits SET restriction_id=?,detected_at=?,signal_source=? WHERE harness_type=?`, c.ID, now.Format(time.RFC3339Nano), wait.Source, c.Harness)
				drafts = append(drafts, events.Draft{Type: "harness.credits_exhausted", Payload: wait})
			} else {
				d, e := saveHarnessLimit(ctx, tx, *wait)
				err = e
				if d != nil {
					drafts = append(drafts, *d)
				}
			}
			if err != nil {
				return nil, err
			}
		}
		proven = proven && p.Status == "running"
		d, err := finishCreditProbe(ctx, tx, p, now, proven)
		if err != nil {
			return nil, err
		}
		drafts = append(drafts, d)
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM credit_probes WHERE id=?`, c.ID).Scan(&status); err != nil {
			return nil, err
		}
		if proven && status != "recovered" {
			result = "A newer restriction remains; this earlier response did not recover the harness. Refresh status."
		}
		_, err = tx.ExecContext(ctx, `UPDATE harness_checks SET status=?,result=?,ended_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, status, result, c.ID)
		c.Status, c.Result = status, result
		drafts = append(drafts, events.Draft{Type: "harness.check_completed", Payload: c})
		return drafts, err
	})
	if errors.Is(err, errHarnessUnchanged) {
		return nil
	}
	return err
}

// ReportHarnessCheckIssue preserves ownership when execution status is ambiguous.
func (w *Workflow) ReportHarnessCheckIssue(ctx context.Context, c HarnessCheck, result string) error {
	_, err := w.events.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		updated, err := tx.ExecContext(ctx, `UPDATE harness_checks SET result=? WHERE id=? AND status IN ('reserved','running') AND result!=?`, result, c.ID, result)
		if err != nil {
			return events.Draft{}, err
		}
		count, err := updated.RowsAffected()
		if err != nil {
			return events.Draft{}, err
		}
		if count == 0 {
			return events.Draft{}, errHarnessUnchanged
		}
		c.Result = result
		return events.Draft{Type: "harness.check_updated", Payload: c}, nil
	})
	if errors.Is(err, errHarnessUnchanged) {
		return nil
	}
	return err
}
