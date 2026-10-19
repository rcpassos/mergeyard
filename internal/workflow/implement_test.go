package workflow_test

import (
	"context"
	"testing"

	"github.com/rcpassos/mergeyard/internal/contract"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// The dashboard's implement criteria and verification sections follow the
// version's contract rather than hard-coded version comparisons.
func TestImplementReportContractPresenceByVersion(t *testing.T) {
	for version := 1; version <= contract.CurrentVersion; version++ {
		criteria, verification := version >= 3, version >= 4
		report := workflow.ImplementReport{SchemaVersion: version}
		if got := report.HasCriteria(); got != criteria {
			t.Fatalf("implement v%d HasCriteria = %v, want %v", version, got, criteria)
		}
		if got := report.HasVerification(); got != verification {
			t.Fatalf("implement v%d HasVerification = %v, want %v", version, got, verification)
		}
	}
}

func TestImplementationHistoryKeepsRecordedReportsAndSessions(t *testing.T) {
	ctx := context.Background()
	w, _, db := newWorkflow(t)
	run, err := w.Transition(ctx, "implementation-history", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE runs SET implementer_session_id='active-control' WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	// Each attempt owns its report; a system step's report must not surface.
	if _, err := db.Exec(`INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status,session_id,step,report_json,report_eligible) VALUES
 ('old',?,'implement','implementer',0,1,'claude','failed',NULL,'','{"schema_version":2,"human_action":null,"status":"failed","summary":"Legacy evidence"}',1),
 ('second',?,'implement','implementer',0,2,'claude','failed','recorded-second','','{"schema_version":3,"human_action":null,"status":"failed","summary":"Second report","criteria":[{"id":"AC1","met":false,"evidence":"Second implementation failed its test"}]}',0),
 ('active',?,'implement','implementer',0,3,'claude','running','recorded-third','',NULL,0),
 ('setup',?,'implement','system',0,9,'claude','succeeded',NULL,'setup','{"schema_version":4,"human_action":null,"status":"failed","summary":"System attempt must not surface"}',0)`, run.ID, run.ID, run.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	history, err := workflow.LoadImplementHistory(ctx, db, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 {
		t.Fatalf("agent history contains system attempts: %+v", history)
	}
	if !(history[0].Sequence < history[1].Sequence && history[1].Sequence < history[2].Sequence) {
		t.Fatalf("attempt order lost: %+v", history)
	}
	if history[0].Attempt != 1 || history[0].SessionID != "" || history[0].Report == nil || history[0].Report.SchemaVersion != 2 || len(history[0].Report.Criteria) != 0 || !history[0].ReportEligible {
		t.Fatalf("legacy report changed: %+v", history[0])
	}
	if history[1].SessionID != "recorded-second" || history[1].Report == nil || len(history[1].Report.Criteria) != 1 || history[1].Report.Criteria[0].Evidence != "Second implementation failed its test" || history[1].ReportEligible {
		t.Fatalf("second attempt lost its report or eligibility: %+v", history[1])
	}
	if history[2].Attempt != 3 || history[2].SessionID != "recorded-third" || history[2].Report != nil || history[2].ReportEligible {
		t.Fatalf("active attempt borrowed another attempt's report: %+v", history[2])
	}
	current, err := workflow.LoadImplementSnapshot(ctx, db, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Attempt != 3 || current.SessionID != "active-control" || current.Report != nil || current.ReportEligible {
		t.Fatalf("active controls changed: %+v", current)
	}
}
