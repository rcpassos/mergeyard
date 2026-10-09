package workflow

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
)

// CreditProbe pins explicit human authorization to a restriction and one physical
// execution. Only that execution can prove recovery; history survives release.
type CreditProbe struct {
	ID            string `json:"id"`
	Harness       string `json:"harness"`
	RestrictionID string `json:"restriction_id"`
	RunID         string `json:"run_id"`
	AttemptID     string `json:"attempt_id,omitempty"`
	Status        string `json:"status"`
	RequestedAt   string `json:"requested_at"`
	EndedAt       string `json:"ended_at,omitempty"`
}

func LoadCreditProbes(ctx context.Context, db queryer, runID string) ([]CreditProbe, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,harness,restriction_id,run_id,COALESCE(attempt_id,''),status,requested_at,COALESCE(ended_at,'') FROM credit_probes WHERE run_id=? ORDER BY requested_at,rowid`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []CreditProbe
	for rows.Next() {
		var p CreditProbe
		if err := rows.Scan(&p.ID, &p.Harness, &p.RestrictionID, &p.RunID, &p.AttemptID, &p.Status, &p.RequestedAt, &p.EndedAt); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

func (p CreditProbe) reserve(ctx context.Context, tx *sql.Tx, runID string) error {
	if p.ID == "" || p.RunID != runID {
		return invalid("Credit probe requires the selected run")
	}
	result, err := tx.ExecContext(ctx, `UPDATE harness_limits SET probe_id=? WHERE harness_type=? AND reason='credits_exhausted' AND restriction_id=? AND probe_id=''`, p.ID, p.Harness, p.RestrictionID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return &fault.Error{Code: "harness.probe_unavailable", Message: "Another recovery probe owns the harness or its restriction changed; refresh before retrying"}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO credit_probes(id,harness,restriction_id,run_id,status) VALUES(?,?,?,?,'reserved')`, p.ID, p.Harness, p.RestrictionID, runID)
	return err
}

// BindCreditProbe is called in the attempt launch transaction, before process
// startup. A restart observes that attempt rather than assigning a second one.
func BindCreditProbe(ctx context.Context, tx *sql.Tx, runID, harness, attemptID string, now time.Time) error {
	limit, err := LoadHarnessLimit(ctx, tx, harness)
	if err != nil {
		return err
	}
	if limit == nil || limit.Reason != "credits_exhausted" {
		return nil
	}
	if limit.ProbeRunID != runID || limit.ProbeAttemptID != "" || now.Before(limit.ResetAt) {
		return &fault.Error{Code: "harness.probe_unavailable", Message: "Credit restriction changed before launch; explicitly select a recovery probe"}
	}

	_, err = tx.ExecContext(ctx, `UPDATE credit_probes SET attempt_id=?,status='running' WHERE run_id=? AND harness=? AND status='reserved' AND id=(SELECT probe_id FROM harness_limits WHERE harness_type=?)`, attemptID, runID, harness, harness)
	return err
}

// RecordCreditProof commits proof and availability together. An old attempt or
// a new restriction cannot reuse earlier successful completion evidence.
func (w *Workflow) RecordCreditProof(ctx context.Context, runID, attemptID string, now time.Time) error {
	_, err := w.events.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		var p CreditProbe
		err := tx.QueryRowContext(ctx, `SELECT id,harness,restriction_id FROM credit_probes WHERE run_id=? AND attempt_id=? AND status='running'`, runID, attemptID).Scan(&p.ID, &p.Harness, &p.RestrictionID)
		if errors.Is(err, sql.ErrNoRows) {
			return events.Draft{}, errHarnessUnchanged
		}
		if err != nil {
			return events.Draft{}, err
		}
		status, event := "released", "harness.probe_released"
		limit, err := LoadHarnessLimit(ctx, tx, p.Harness)
		if err != nil {
			return events.Draft{}, err
		}
		if limit != nil && limit.Reason == "credits_exhausted" && limit.RestrictionID == p.RestrictionID && limit.ProbeID == p.ID {
			var result sql.Result
			if limit.ResetAt.IsZero() {
				result, err = tx.ExecContext(ctx, `DELETE FROM harness_limits WHERE harness_type=? AND restriction_id=? AND probe_id=?`, p.Harness, p.RestrictionID, p.ID)
				event = "harness.available"
			} else {
				reset := limit.ResetAt.Format(time.RFC3339Nano)
				marker := reset
				event = "harness.credit_recovered"
				if !now.Before(limit.ResetAt) {
					marker = "available:" + reset
					event = "harness.available"
				}
				result, err = tx.ExecContext(ctx, `UPDATE harness_limits SET reason='temporary_limit',restriction_id='',probe_id='',notified_until=? WHERE harness_type=? AND restriction_id=? AND probe_id=?`, marker, p.Harness, p.RestrictionID, p.ID)
			}
			if err != nil {
				return events.Draft{}, err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return events.Draft{}, err
			}
			if n == 1 {
				status = "recovered"
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE harness_limits SET probe_id='' WHERE harness_type=? AND probe_id=?`, p.Harness, p.ID); err != nil {
			return events.Draft{}, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE credit_probes SET status=?,ended_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, status, p.ID)
		p.RunID, p.AttemptID, p.Status = runID, attemptID, status
		return events.Draft{RunID: runID, Type: event, Payload: p}, err
	})
	if errors.Is(err, errHarnessUnchanged) {
		return nil
	}
	return err
}

// reconcileCreditProbes releases only ended executions and unused selections.
// A running attempt keeps ownership even when its run requires attention.
func reconcileCreditProbes(ctx context.Context, tx *sql.Tx, runID string) ([]events.Draft, error) {
	rows, err := tx.QueryContext(ctx, `SELECT p.id,p.harness,p.restriction_id,p.run_id,COALESCE(p.attempt_id,''),p.status,p.requested_at
 FROM credit_probes p JOIN runs r ON r.id=p.run_id LEFT JOIN phase_attempts a ON a.id=p.attempt_id
 WHERE (?='' OR p.run_id=?) AND (
  (p.status='running' AND a.status!='running') OR
  (p.status='reserved' AND (r.stop_requested=1 OR r.state NOT IN ('ACTIVE','WAITING_FOR_HARNESS') OR
   COALESCE(r.current_phase,'') != COALESCE((SELECT json_extract(snapshot_json,'$.next_phase') FROM run_retries WHERE run_id=p.run_id AND pending=0 AND COALESCE(json_extract(snapshot_json,'$.error'),'')='' ORDER BY id DESC LIMIT 1),r.current_phase,''))))`, runID, runID)
	if err != nil {
		return nil, err
	}
	var probes []CreditProbe
	for rows.Next() {
		var p CreditProbe
		if err := rows.Scan(&p.ID, &p.Harness, &p.RestrictionID, &p.RunID, &p.AttemptID, &p.Status, &p.RequestedAt); err != nil {
			rows.Close()
			return nil, err
		}
		probes = append(probes, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var drafts []events.Draft
	for _, p := range probes {
		if _, err := tx.ExecContext(ctx, `UPDATE credit_probes SET status='released',ended_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, p.ID); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE harness_limits SET probe_id='' WHERE harness_type=? AND probe_id=?`, p.Harness, p.ID); err != nil {
			return nil, err
		}
		p.Status = "released"
		drafts = append(drafts, events.Draft{RunID: p.RunID, Type: "harness.probe_released", Payload: p})
	}
	return drafts, nil
}

// ReconcileCreditProbes repairs an interrupted attempt-ending operation before
// another operation can reserve a probe. It never releases a live execution.
func (w *Workflow) ReconcileCreditProbes(ctx context.Context) error {
	_, err := w.events.CommitBatch(ctx, func(tx *sql.Tx) ([]events.Draft, error) {
		drafts, err := reconcileCreditProbes(ctx, tx, "")
		if err == nil && len(drafts) == 0 {
			return nil, errHarnessUnchanged
		}
		return drafts, err
	})
	if errors.Is(err, errHarnessUnchanged) {
		return nil
	}
	return err
}

func saveCreditBlock(ctx context.Context, tx *sql.Tx, v HarnessWait) (*events.Draft, error) {
	// Each newly classified interruption supersedes earlier proof, including an
	// already running probe. Its ownership remains until completion or Stop, so
	// another run cannot start a second probe. Unlaunched reservations can be
	// revoked. Attempt identity is also the restriction generation.
	if _, err := tx.ExecContext(ctx, `UPDATE credit_probes SET status='released',ended_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE harness=? AND status='reserved'`, v.Harness); err != nil {
		return nil, err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO harness_limits(harness_type,limited_until,reset_time_source,detected_at,phase_attempt_id,signal_source,reason,restriction_id) VALUES(?,NULL,'',?,?,?,'credits_exhausted',?) ON CONFLICT(harness_type) DO UPDATE SET detected_at=excluded.detected_at,phase_attempt_id=excluded.phase_attempt_id,signal_source=excluded.signal_source,reason='credits_exhausted',restriction_id=excluded.restriction_id,probe_id=CASE WHEN EXISTS(SELECT 1 FROM credit_probes WHERE id=harness_limits.probe_id AND status='running') THEN harness_limits.probe_id ELSE '' END,notified_until=''`, v.Harness, v.DetectedAt.Format(time.RFC3339Nano), v.AttemptID, v.Source, v.AttemptID)
	return &events.Draft{Type: "harness.credits_exhausted", Payload: v}, err
}
