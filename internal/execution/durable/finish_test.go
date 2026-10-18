package durable_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/execution/durable"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/verification"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// Completion stores the implement report on the attempt, freezes its
// publication eligibility, and records the phase on the event, so readers never
// infer the phase from a missing key.
func TestFinishImplementStoresReportAndPhaseOnAttempt(t *testing.T) {
	e, _, run, _ := executor(t)
	ctx := context.Background()
	if _, err := e.DB.ExecContext(ctx, `INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status,step) VALUES
	 ('attempt-1',?,'implement','implementer',0,1,'claude','running',''),
	 ('attempt-2',?,'implement','implementer',0,2,'claude','running',''),
	 ('attempt-3',?,'implement','implementer',0,3,'claude','running','')`, run.ID, run.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	result := harness.PhaseResult{SchemaVersion: 4, Status: "success", Summary: "Added feature", Verification: []verification.Entry{{Command: "go test", Outcome: "passed"}}}
	if err := e.FinishImplement(ctx, run.ID, durable.Attempt{ID: "attempt-1", Number: 1}, nil, result, nil); err != nil {
		t.Fatal(err)
	}
	// A parse failure has no report to store; the attempt records the error.
	cause := &fault.Error{Code: "phase.result_invalid", Message: "Structured output must be an implement result object"}
	if err := e.FinishImplement(ctx, run.ID, durable.Attempt{ID: "attempt-2", Number: 2}, nil, harness.PhaseResult{}, cause); err != nil {
		t.Fatal(err)
	}
	// A parsed report rejected for mismatched criteria stays visible to the
	// dashboard but is not publishable.
	if err := e.FinishImplement(ctx, run.ID, durable.Attempt{ID: "attempt-3", Number: 3}, nil, result, cause); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		id           string
		status       string
		version      int
		summary      string
		verification int
		eligible     int
	}{
		{"attempt-1", "succeeded", 4, "Added feature", 1, 1},
		{"attempt-2", "failed", 0, "", 0, 0},
		{"attempt-3", "failed", 4, "Added feature", 1, 0},
	}
	for _, tc := range checks {
		var status string
		var report sql.NullString
		var eligible int
		if err := e.DB.QueryRowContext(ctx, `SELECT status,report_json,report_eligible FROM phase_attempts WHERE id=?`, tc.id).Scan(&status, &report, &eligible); err != nil {
			t.Fatal(err)
		}
		var saved workflow.ImplementReport
		if report.Valid {
			if err := json.Unmarshal([]byte(report.String), &saved); err != nil {
				t.Fatal(err)
			}
		}
		if status != tc.status || saved.SchemaVersion != tc.version || saved.Summary != tc.summary || len(saved.Verification) != tc.verification || eligible != tc.eligible {
			t.Fatalf("%s = %q report(v%d %q, %d entries) eligible %d, want %q report(v%d %q, %d entries) eligible %d", tc.id, status, saved.SchemaVersion, saved.Summary, len(saved.Verification), eligible, tc.status, tc.version, tc.summary, tc.verification, tc.eligible)
		}
	}
	rows, err := e.DB.QueryContext(ctx, `SELECT payload_json FROM events WHERE run_id=? AND type IN ('phase.completed','phase.failed') ORDER BY id`, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var payloads []string
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, payload)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(payloads) != 3 || !strings.Contains(payloads[0], `"phase":"implement"`) || !strings.Contains(payloads[0], `"summary":"Added feature"`) {
		t.Fatalf("completion events lost the phase or report: %v", payloads)
	}
	if !strings.Contains(payloads[1], `"phase":"implement"`) || !strings.Contains(payloads[1], `"error":"phase.result_invalid:`) {
		t.Fatalf("failed completion event lost the phase or error: %v", payloads)
	}
}
