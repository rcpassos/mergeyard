package workflow_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rcpassos/mergeyard/internal/review"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestPriorContextIncludesPartialFixDisputesAfterManualWork(t *testing.T) {
	w, _, db := newWorkflow(t)
	ctx := context.Background()
	if _, err := w.Transition(ctx, "partial-fix", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7}); err != nil {
		t.Fatal(err)
	}
	prior := review.Report{SchemaVersion: 2, Status: "changes_required", Findings: []review.Finding{{ID: "F1", Severity: "blocking", Title: "Bug", Details: "Broken"}}, PriorFindings: []review.PriorFinding{}}
	fix := review.FixReport{SchemaVersion: 1, Status: "blocked", Summary: "Need input on remaining work", Responses: []review.Response{{FindingID: "F1", Resolution: "disputed", Note: "Already safe"}}}
	encodedPrior, _ := json.Marshal(prior)
	encodedFix, _ := json.Marshal(fix)
	if _, err := db.Exec(`INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status) VALUES ('r','partial-fix','review','reviewer',1,1,'claude','succeeded'),('f','partial-fix','fix','implementer',1,1,'claude','failed');
	 INSERT INTO review_attempts(attempt_id,target_sha,diff,snapshot_json,session_id,permission_mode,allowed_tools_json,restored,accepted,report_json) VALUES ('r','pinned','','{}','reviewer','auto','[]',1,1,?)`, string(encodedPrior)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO fix_attempts(attempt_id,target_sha,session_id,findings_json,permission_mode,allowed_tools_json,report_json) VALUES ('f','pinned','implementer','[]','auto','[]',?)`, string(encodedFix)); err != nil {
		t.Fatal(err)
	}
	findings, partial, err := review.LoadPriorContext(ctx, db, "partial-fix", 2)
	if err != nil || len(findings) != 1 || partial == nil || len(partial.Responses) != 1 || partial.Responses[0].Resolution != "disputed" {
		t.Fatalf("partial dispute lost: %+v %+v %v", findings, partial, err)
	}
	current := review.Report{SchemaVersion: 2, Status: "approved", Findings: []review.Finding{}, PriorFindings: []review.PriorFinding{{FindingID: "F1", Outcome: "fixed", Note: "Dismissed without ruling"}}}
	if err := current.ValidatePriorFindings(2, findings, partial); err == nil {
		t.Fatal("partial fix dispute was dropped without a ruling")
	}
	current.PriorFindings[0].Outcome = "no_longer_applies"
	if err := current.ValidatePriorFindings(2, findings, partial); err != nil {
		t.Fatal(err)
	}
}
