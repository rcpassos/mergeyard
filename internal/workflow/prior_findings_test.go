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
	if _, err := db.Exec(`INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status,report_eligible) VALUES ('r','partial-fix','review','reviewer',1,1,'claude','succeeded',0),('f','partial-fix','fix','implementer',1,1,'claude','failed',1);
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

func TestAcceptedRetryRetiresOldDisputesButKeepsNewFixResponses(t *testing.T) {
	w, _, db := newWorkflow(t)
	ctx := context.Background()
	if _, err := w.Transition(ctx, "dispute-retry", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7}); err != nil {
		t.Fatal(err)
	}
	blocker := review.Finding{ID: "F1", Severity: "blocking", Title: "Bug", Details: "Still broken"}
	prior := review.Report{SchemaVersion: 2, Status: "changes_required", Findings: []review.Finding{blocker}, PriorFindings: []review.PriorFinding{}}
	fix := review.FixReport{SchemaVersion: 1, Status: "blocked", Summary: "Partial fix", Responses: []review.Response{{FindingID: "F1", Resolution: "disputed", Note: "Original dispute"}}}
	insertReview := func(id string, attempt int, report review.Report) {
		t.Helper()
		raw, _ := json.Marshal(report)
		if _, err := db.Exec(`INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status) VALUES (?,'dispute-retry','review','reviewer',1,?,'claude','succeeded')`, id, attempt); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO review_attempts(attempt_id,target_sha,diff,snapshot_json,session_id,permission_mode,allowed_tools_json,restored,accepted,report_json) VALUES (?,'same-head','','{}','reviewer','auto','[]',1,1,?)`, id, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	fixTarget := "same-head"
	insertFix := func(id string, attempt int) {
		t.Helper()
		raw, _ := json.Marshal(fix)
		if _, err := db.Exec(`INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status,report_eligible) VALUES (?,'dispute-retry','fix','implementer',1,?,'claude','failed',1)`, id, attempt); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO fix_attempts(attempt_id,target_sha,session_id,findings_json,permission_mode,allowed_tools_json,report_json) VALUES (?,?,'implementer','[]','auto','[]',?)`, id, fixTarget, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	insertReview("review-1", 1, prior)
	insertFix("fix-1", 1)
	prior.PriorFindings = []review.PriorFinding{{FindingID: "F1", Outcome: "not_fixed", Note: "Dispute rejected"}}
	insertReview("review-2", 2, prior)
	findings, contextFix, err := review.LoadPriorContext(ctx, db, "dispute-retry", 1)
	if err != nil {
		t.Fatal(err)
	}
	approval := review.Report{SchemaVersion: 2, Status: "approved", Findings: []review.Finding{}, PriorFindings: []review.PriorFinding{{FindingID: "F1", Outcome: "fixed", Note: "Manual repair verified"}}}
	if err := approval.ValidatePriorFindings(1, findings, contextFix); err != nil {
		t.Fatalf("adjudicated dispute replayed: %v", err)
	}
	if contextFix != nil {
		t.Fatal("accepted Retry retained the old fix context")
	}
	// A subsequent fix against that same SHA is new context, even if it disputes
	// the same finding. It must not be mistaken for the adjudicated old response.
	fix.Responses[0].Note = "Renewed dispute"
	insertFix("fix-2", 2)
	findings, contextFix, err = review.LoadPriorContext(ctx, db, "dispute-retry", 1)
	if err != nil || contextFix == nil || contextFix.Responses[0].Note != "Renewed dispute" {
		t.Fatalf("new dispute lost: %+v %v", contextFix, err)
	}
	fixTarget = "other-head"
	fix.Responses[0].Note = "Unrelated target"
	insertFix("fix-3", 3)
	findings, contextFix, err = review.LoadPriorContext(ctx, db, "dispute-retry", 1)
	if err != nil || contextFix == nil || contextFix.Responses[0].Note != "Renewed dispute" {
		t.Fatalf("unrelated target replaced selected review's fix context: %+v %v", contextFix, err)
	}
	if err := approval.ValidatePriorFindings(1, findings, contextFix); err == nil {
		t.Fatal("new dispute accepted without a ruling")
	}
}

// A fix whose completion was never publishable must not supply dispute context
// either, matching the verification evidence publication exposes.
func TestPriorContextSkipsIneligibleFixReport(t *testing.T) {
	w, _, db := newWorkflow(t)
	ctx := context.Background()
	if _, err := w.Transition(ctx, "ineligible-fix", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7}); err != nil {
		t.Fatal(err)
	}
	prior := review.Report{SchemaVersion: 2, Status: "changes_required", Findings: []review.Finding{{ID: "F1", Severity: "blocking", Title: "Bug", Details: "Broken"}}, PriorFindings: []review.PriorFinding{}}
	fix := review.FixReport{SchemaVersion: 1, Status: "success", Summary: "Invalid completion", Responses: []review.Response{{FindingID: "F1", Resolution: "disputed", Note: "Not really fixed"}}}
	encodedPrior, _ := json.Marshal(prior)
	encodedFix, _ := json.Marshal(fix)
	if _, err := db.Exec(`INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status,report_eligible) VALUES ('r','ineligible-fix','review','reviewer',1,1,'claude','succeeded',0),('f','ineligible-fix','fix','implementer',1,1,'claude','failed',0);
	 INSERT INTO review_attempts(attempt_id,target_sha,diff,snapshot_json,session_id,permission_mode,allowed_tools_json,restored,accepted,report_json) VALUES ('r','pinned','','{}','reviewer','auto','[]',1,1,?)`, string(encodedPrior)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO fix_attempts(attempt_id,target_sha,session_id,findings_json,permission_mode,allowed_tools_json,report_json) VALUES ('f','pinned','implementer','[]','auto','[]',?)`, string(encodedFix)); err != nil {
		t.Fatal(err)
	}
	findings, partial, err := review.LoadPriorContext(ctx, db, "ineligible-fix", 2)
	if err != nil || len(findings) != 1 || partial != nil {
		t.Fatalf("ineligible fix reached review context: %+v %+v %v", findings, partial, err)
	}
}
