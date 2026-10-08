package workflow

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/rcpassos/mergeyard/internal/events"
)

// HarnessLimit is the account-wide restriction authority. Expired timed records
// remain so earlier cooldown observations still follow a later reported reset.
// This component owns every harness_limits write and its availability events.
type HarnessLimit struct {
	Reason          string
	RestrictionID   string
	ProbeID         string
	ProbeRunID      string
	ProbeAttemptID  string
	Harness         string
	ResetAt         time.Time
	ResetTimeSource string
	Source          string
	notifiedUntil   string
}

func LoadHarnessLimit(ctx context.Context, db queryer, harness string) (*HarnessLimit, error) {
	v := HarnessLimit{Harness: harness}
	var reset string
	err := db.QueryRowContext(ctx, "SELECT COALESCE(limited_until,''),reset_time_source,signal_source,notified_until,reason,restriction_id,probe_id,COALESCE((SELECT run_id FROM credit_probes WHERE id=probe_id),''),COALESCE((SELECT attempt_id FROM credit_probes WHERE id=probe_id),'') FROM harness_limits WHERE harness_type=?", harness).Scan(&reset, &v.ResetTimeSource, &v.Source, &v.notifiedUntil, &v.Reason, &v.RestrictionID, &v.ProbeID, &v.ProbeRunID, &v.ProbeAttemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if reset == "" {
		return &v, nil
	}
	v.ResetAt, err = time.Parse(time.RFC3339Nano, reset)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// CurrentHarnessWait preserves the observation history while presenting the
// reset that actually governs the waiting run, including after expiry/restart.
func CurrentHarnessWait(ctx context.Context, db queryer, history []HarnessWait) (*HarnessWait, error) {
	if len(history) == 0 {
		return nil, nil
	}
	v := history[len(history)-1]
	limit, err := LoadHarnessLimit(ctx, db, v.Harness)
	if err != nil {
		return nil, err
	}
	if limit != nil {
		v.Reason = limit.Reason
		v.ResetAt, v.ResetTimeSource, v.Source = limit.ResetAt, limit.ResetTimeSource, limit.Source
	}
	return &v, nil
}

func saveHarnessLimit(ctx context.Context, tx *sql.Tx, v HarnessWait) (*events.Draft, error) {
	previous, err := LoadHarnessLimit(ctx, tx, v.Harness)
	if err != nil {
		return nil, err
	}
	if previous != nil && previous.ResetAt.After(v.DetectedAt) {
		// Reliable reported evidence takes precedence over an estimated cooldown in
		// either arrival order. Within the same source class, keep the later reset.
		if previous.ResetTimeSource == "reported" && v.ResetTimeSource == "default_cooldown" {
			return nil, nil
		}
		if previous.ResetTimeSource == v.ResetTimeSource && !v.ResetAt.After(previous.ResetAt) {
			return nil, nil
		}
	}
	reset := v.ResetAt.Format(time.RFC3339Nano)
	if previous != nil && previous.Reason == "credits_exhausted" {
		_, err = tx.ExecContext(ctx, `UPDATE harness_limits SET limited_until=?,reset_time_source=?,signal_source=? WHERE harness_type=?`, reset, v.ResetTimeSource, v.Source, v.Harness)
		return &events.Draft{Type: "harness.usage_limited", Payload: v}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO harness_limits(harness_type,limited_until,reset_time_source,detected_at,phase_attempt_id,signal_source,notified_until) VALUES(?,?,?,?,?,?,?) ON CONFLICT(harness_type) DO UPDATE SET limited_until=excluded.limited_until,reset_time_source=excluded.reset_time_source,detected_at=excluded.detected_at,phase_attempt_id=excluded.phase_attempt_id,signal_source=excluded.signal_source,notified_until=excluded.notified_until`, v.Harness, reset, v.ResetTimeSource, v.DetectedAt.Format(time.RFC3339Nano), v.AttemptID, v.Source, reset)
	if err != nil {
		return nil, err
	}
	return &events.Draft{Type: "harness.usage_limited", Payload: map[string]string{"harness": v.Harness, "reset_at": reset, "reset_time_source": v.ResetTimeSource, "source": v.Source}}, nil
}

var errHarnessUnchanged = errors.New("harness availability unchanged")

// ReconcileHarnessLimits records expiration once, and recovers unacknowledged
// availability events from older data. Mutations and their events share a commit.
func (w *Workflow) ReconcileHarnessLimits(ctx context.Context, now time.Time) error {
	rows, err := w.db.QueryContext(ctx, "SELECT harness_type FROM harness_limits ORDER BY harness_type")
	if err != nil {
		return err
	}
	var harnesses []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		harnesses = append(harnesses, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, name := range harnesses {
		for {
			_, err := w.events.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
				current, err := LoadHarnessLimit(ctx, tx, name)
				if err != nil {
					return events.Draft{}, err
				}
				if current == nil || current.Reason == "credits_exhausted" {
					return events.Draft{}, errHarnessUnchanged
				}
				reset := current.ResetAt.Format(time.RFC3339Nano)
				availableMarker := "available:" + reset
				event, marker := "harness.usage_limited", reset
				if current.notifiedUntil == reset || current.notifiedUntil == availableMarker {
					if now.Before(current.ResetAt) || current.notifiedUntil == availableMarker {
						return events.Draft{}, errHarnessUnchanged
					}
					event, marker = "harness.available", availableMarker
				}
				_, err = tx.ExecContext(ctx, "UPDATE harness_limits SET notified_until=? WHERE harness_type=?", marker, name)
				return events.Draft{Type: event, Payload: map[string]string{"harness": name, "reset_at": reset, "reset_time_source": current.ResetTimeSource, "source": current.Source}}, err
			})
			if errors.Is(err, errHarnessUnchanged) {
				break
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}
