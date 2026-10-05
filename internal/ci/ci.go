// Package ci retains commit-specific CI evidence and durable readiness intent.
package ci

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type Check struct {
	Name       string `json:"name"`
	SHA        string `json:"sha"`
	Source     string `json:"source"`
	AppID      int64  `json:"app_id,omitempty"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
	URL        string `json:"url,omitempty"`
	Excerpt    string `json:"excerpt,omitempty"`
}
type Requirement struct {
	Name  string `json:"name"`
	AppID int64  `json:"app_id,omitempty"`
}
type Evidence struct {
	SHA                 string        `json:"sha"`
	MergeSHA            string        `json:"merge_sha,omitempty"`
	Checks              []Check       `json:"checks"`
	Required            []Requirement `json:"required"`
	AllowSkippedNeutral bool          `json:"allow_skipped_neutral"`
}
type Snapshot struct {
	SHA          string    `json:"sha"`
	StartedAt    time.Time `json:"started_at"`
	Deadline     time.Time `json:"deadline"`
	CurrentHead  string    `json:"current_head,omitempty"`
	Evidence     Evidence  `json:"evidence"`
	QueryError   string    `json:"query_error,omitempty"`
	ReadyStarted bool      `json:"ready_started"`
	Warning      string    `json:"warning,omitempty"`
	RepairCause  string    `json:"repair_cause,omitempty"`
}

// Normalize retains native outcomes; unrecognized or stale evidence never passes.
func Normalize(c Check, allowSkippedNeutral bool) string {
	if c.Source == "status" {
		switch c.Conclusion {
		case "success":
			return "passed"
		case "pending":
			return "pending"
		case "failure", "error":
			return "failed"
		default:
			return "unknown"
		}
	}
	switch c.Status {
	case "queued", "in_progress", "requested", "waiting", "pending":
		return "pending"
	case "completed":
	default:
		return "unknown"
	}
	switch c.Conclusion {
	case "success":
		return "passed"
	case "skipped", "neutral":
		if allowSkippedNeutral {
			return "passed"
		}
	case "failure", "timed_out", "startup_failure", "cancelled", "action_required":
		return "failed"
	}
	return "unknown"
}

// Gate requires every reported outcome and every required source to pass.
// Terminal failures are repairable; cancellations and action requests take priority.
func (e *Evidence) Gate() (passed bool, attention string) {
	passed = true
	requiredSHA := e.SHA
	if e.MergeSHA != "" {
		for _, c := range e.Checks {
			if c.SHA == e.MergeSHA {
				requiredSHA = e.MergeSHA
				break
			}
		}
	}

	for i := range e.Checks {
		c := &e.Checks[i]
		c.State = Normalize(*c, e.AllowSkippedNeutral)
		validSHA := c.SHA == e.SHA || (e.MergeSHA != "" && c.SHA == e.MergeSHA)
		if !validSHA {
			c.State = "unknown"
		}
		if c.State != "passed" {
			passed = false
		}
		if validSHA && c.State == "failed" {
			switch c.Conclusion {
			case "cancelled", "action_required":
				attention = "ci.action_required"
			case "timed_out":
				if attention == "" {
					attention = "ci.check_timed_out"
				}
			default:
				if attention != "ci.action_required" {
					attention = "ci.check_failed"
				}
			}
		}
	}
	for _, r := range e.Required {
		// GitHub requires both a check run and a legacy status when they share
		// a required context. One source kind must not authorize the other.
		kinds := map[string]bool{}
		for _, c := range e.Checks {
			if c.SHA != requiredSHA || !strings.EqualFold(c.Name, r.Name) {
				continue
			}
			if _, exists := kinds[c.Source]; !exists {
				kinds[c.Source] = false
			}
			if (r.AppID <= 0 || c.AppID == r.AppID) && c.State == "passed" {
				kinds[c.Source] = true
			}
		}
		if len(kinds) == 0 {
			passed = false
		}
		for _, satisfied := range kinds {
			if !satisfied {
				passed = false
			}
		}
	}
	return
}

type Queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func Load(ctx context.Context, db Queryer, id string) (*Snapshot, error) {
	var data string
	err := db.QueryRowContext(ctx, "SELECT snapshot_json FROM ci_waits WHERE run_id=?", id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s Snapshot
	err = json.Unmarshal([]byte(data), &s)
	return &s, err
}
func Save(ctx context.Context, tx *sql.Tx, id string, s Snapshot) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO ci_waits(run_id,snapshot_json) VALUES(?,?) ON CONFLICT(run_id) DO UPDATE SET snapshot_json=excluded.snapshot_json`, id, string(data))
	return err
}
