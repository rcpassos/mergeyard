package workflow_test

import (
	"context"
	"testing"

	"github.com/rcpassos/mergeyard/internal/review"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestReviewCompletionIsAtomicAndRequiresRestoration(t *testing.T) {
	w, bus, db := newWorkflow(t)
	ctx := context.Background()
	round := 1
	for _, req := range []workflow.Request{{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7}, {Trigger: workflow.ClaimSucceeded}, {Trigger: workflow.WorktreeReady}, {Trigger: workflow.ImplementSucceeded, Metadata: workflow.MetadataPatch{ReviewRound: &round}}} {
		if _, err := w.Transition(ctx, "review-run", req); err != nil {
			t.Fatal(err)
		}
	}
	_, err := db.Exec(`INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status) VALUES ('a','review-run','review','reviewer',1,1,'claude','running');
 INSERT INTO review_attempts(attempt_id,target_sha,diff,snapshot_json,session_id,permission_mode,allowed_tools_json) VALUES ('a','pinned','','{}','independent','auto','[]')`)
	if err != nil {
		t.Fatal(err)
	}
	sha := "pinned"
	completion := workflow.ReviewCompletion{AttemptID: "a", Report: review.Report{SchemaVersion: 1, Status: "approved", Summary: "Clean", Findings: []review.Finding{}}}
	req := workflow.Request{Trigger: workflow.ReviewApproved, Metadata: workflow.MetadataPatch{ApprovedSHA: &sha, Review: &completion}}
	if _, err := w.Transition(ctx, "review-run", req); err == nil {
		t.Fatal("accepted verdict before restoring")
	}
	run, _ := w.Get(ctx, "review-run")
	if run.State != workflow.Active || run.ApprovedSHA != "" || run.Review.Report != nil {
		t.Fatalf("failed completion leaked: %+v", run)
	}
	if _, err := db.Exec("UPDATE review_attempts SET restored=1 WHERE attempt_id='a'"); err != nil {
		t.Fatal(err)
	}
	// A failed event write rolls back the report, approval and attempt together.
	if _, err := db.Exec(`CREATE TRIGGER reject_review BEFORE INSERT ON events WHEN NEW.type='review.completed' BEGIN SELECT RAISE(FAIL,'event blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Transition(ctx, "review-run", req); err == nil {
		t.Fatal("event rejection accepted verdict")
	}
	run, _ = w.Get(ctx, "review-run")
	if run.State != workflow.Active || run.Review.Report != nil {
		t.Fatal("event rejection leaked review")
	}
	db.Exec("DROP TRIGGER reject_review")
	run, err = w.Transition(ctx, "review-run", req)
	if err != nil || run.State != workflow.WaitingForCI || run.ApprovedSHA != sha || !run.Review.Accepted || run.Review.Report.Status != "approved" {
		t.Fatalf("completion=%+v err=%v", run, err)
	}
	history, err := bus.History(ctx, 0, 100)
	if err != nil || history[len(history)-1].Type != "review.completed" {
		t.Fatal("missing review event")
	}
}
