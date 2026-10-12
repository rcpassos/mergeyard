package workflow_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestImplementationHistoryKeepsRecordedReportsAndSessions(t *testing.T) {
	ctx := context.Background()
	w, bus, db := newWorkflow(t)
	run, err := w.Transition(ctx, "implementation-history", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE runs SET implementer_session_id='active-control' WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status,session_id,step) VALUES
 ('old',?,'implement','implementer',0,1,'claude','failed',NULL,''),
 ('second',?,'implement','implementer',0,2,'claude','failed','recorded-second',''),
 ('active',?,'implement','implementer',0,3,'claude','running','recorded-third',''),
 ('setup',?,'implement','system',0,9,'claude','succeeded',NULL,'setup')`, run.ID, run.ID, run.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{
		`{"attempt":1,"result":{"schema_version":2,"human_action":null,"status":"failed","summary":"Legacy evidence"}}`,
		`{"phase":"implement","attempt":2,"result":{"schema_version":3,"human_action":null,"status":"failed","summary":"Second report","criteria":[{"id":"AC1","met":false,"evidence":"Second implementation failed its test"}]}}`,
		`{"phase":"fix","attempt":2,"result":{"schema_version":3,"status":"success","summary":"Unrelated fix","criteria":[{"id":"AC1","met":true,"evidence":"Wrong phase"}]}}`,
		`{"phase":"review","attempt":3,"result":{"schema_version":4,"status":"approved","summary":"Unrelated review"}}`,
	} {
		if _, err := bus.Commit(ctx, func(*sql.Tx) (events.Draft, error) {
			return events.Draft{RunID: run.ID, Type: "phase.failed", Payload: json.RawMessage(payload)}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	history, err := workflow.LoadImplementHistory(ctx, db, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 {
		t.Fatalf("agent history contains system attempts: %+v", history)
	}
	if history[0].Attempt != 1 || history[0].SessionID != "" || history[0].Report == nil || history[0].Report.SchemaVersion != 2 || len(history[0].Report.Criteria) != 0 {
		t.Fatalf("legacy report changed: %+v", history[0])
	}
	if history[1].SessionID != "recorded-second" || history[1].Report == nil || len(history[1].Report.Criteria) != 1 || history[1].Report.Criteria[0].Evidence != "Second implementation failed its test" {
		t.Fatalf("second report overwritten by another phase: %+v", history[1])
	}
	if history[2].Attempt != 3 || history[2].SessionID != "recorded-third" || history[2].Report != nil {
		t.Fatalf("active attempt borrowed another phase's report: %+v", history[2])
	}
	current, err := workflow.LoadImplementSnapshot(ctx, db, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Attempt != 3 || current.SessionID != "active-control" || current.Report != nil {
		t.Fatalf("active controls changed: %+v", current)
	}
}
