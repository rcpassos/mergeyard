package workflow_test

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestObservationRetainsApprovalHistoryAndCurrentWait(t *testing.T) {
	w, _, db := newWorkflow(t)
	run := seed(t, w, workflow.Active, workflow.Implement)
	statements := []string{
		`UPDATE runs SET approved_sha='approved-head',implementer_session_id='current-session'`,
		`INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status,session_id) VALUES ('first','run','implement','implementer',0,1,'claude','failed','old-session'),('second','run','implement','implementer',0,2,'claude','succeeded','current-session')`,
		`INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status,step,system_step_json) VALUES ('checks','run','implement','system',0,1,'','succeeded','checks','{"id":"checks","commands":["go test ./..."]}')`,
		`INSERT INTO harness_waits(run_id,snapshot_json) VALUES ('run','{"harness":"claude","reason":"temporary_limit","reset_at":"2026-10-15T10:00:00Z","reset_time_source":"default_cooldown"}')`,
		`INSERT INTO harness_limits(harness_type,limited_until,reset_time_source,signal_source) VALUES ('claude','2026-10-15T12:00:00Z','reported','native')`,
		`INSERT INTO credit_probes(id,harness,restriction_id,run_id,status) VALUES ('probe','claude','restriction','run','recovered')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	observed, err := workflow.ObserveRun(ctx, db, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if observed.ApprovedSHA != "approved-head" || len(observed.ImplementHistory) != 2 || observed.ImplementHistory[0].SessionID != "old-session" || observed.Implementer.SessionID != "current-session" {
		t.Fatalf("lost approval or attempt history: %+v", observed)
	}
	if len(observed.SystemSteps) != 1 || observed.SystemSteps[0].Commands[0] != "go test ./..." || len(observed.CreditProbes) != 1 || observed.CreditProbes[0].Status != "recovered" {
		t.Fatalf("lost system steps or Recovery probes: %+v", observed)
	}
	if len(observed.HarnessWaitHistory) != 1 || observed.HarnessWaitHistory[0].ResetAt.Hour() != 10 || observed.HarnessWaitHistory[0].ResetTimeSource != "default_cooldown" || observed.HarnessWait.ResetAt.Hour() != 12 || observed.HarnessWait.ResetTimeSource != "reported" {
		t.Fatalf("current wait rewrote historical observation: %+v", observed)
	}
	// An expired reported reset remains the governing evidence.
	if !observed.HarnessWait.ResetAt.Before(time.Now()) {
		t.Fatal("fixture reset must have expired")
	}
	saved, err := w.Get(ctx, run.ID)
	if err != nil || !reflect.DeepEqual(saved, observed) {
		t.Fatalf("workflow observation differs: %+v, %v", saved, err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET approved_sha='transaction-head' WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	transactional, err := workflow.ObserveRun(ctx, tx, run.ID)
	if err != nil || transactional.ApprovedSHA != "transaction-head" {
		t.Fatalf("observation lost caller transaction: %+v, %v", transactional, err)
	}
}

// transitionAfterSelection advances the real SQLite workflow at the first
// evidence read, after the selection statement has released its rows.
type transitionAfterSelection struct {
	*sql.DB
	transition func()
}

func (db *transitionAfterSelection) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if transition := db.transition; transition != nil {
		db.transition = nil
		transition()
	}
	return db.DB.QueryRowContext(ctx, query, args...)
}

func TestRecentObservationKeepsPhaseAndExecutionTogetherAcrossTransition(t *testing.T) {
	for _, selectedID := range []string{"", "run"} {
		t.Run("selected="+selectedID, func(t *testing.T) {
			w, _, db := newWorkflow(t)
			seed(t, w, workflow.Active, workflow.Implement)
			if _, err := db.Exec(`UPDATE runs SET implementer_agent='claude',implementer_session_id='implement-session',reviewer_agent='codex',reviewer_session_id='review-session',pr_number=42;
    INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status,model,effort,skills_json,session_id,process_session,log_path)
    VALUES ('implement-attempt','run','implement','implementer',0,1,'claude','succeeded','implement-model','high','["implement-skill"]','implement-session','implement-process','implement.log')`); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			boundary := &transitionAfterSelection{DB: db, transition: func() {
				if _, err := w.Transition(ctx, "run", workflow.Request{Trigger: workflow.ImplementSucceeded}); err != nil {
					t.Fatal(err)
				}
			}}
			observations, err := workflow.ObserveRecentRuns(ctx, boundary, selectedID)
			if err != nil {
				t.Fatal(err)
			}
			if len(observations) != 1 {
				t.Fatalf("observations=%+v", observations)
			}
			before := observations[0]
			if before.Phase != workflow.Implement || before.Attempt != 1 || before.Agent != "claude" || before.Model != "implement-model" || before.Effort != "high" || before.SkillsJSON != `["implement-skill"]` || before.SessionID != "implement-session" || before.ProcessSession != "implement-process" || before.LogPath != "implement.log" {
				t.Fatalf("mixed lifecycle and execution across transition: %+v", before)
			}
			observations, err = workflow.ObserveRecentRuns(ctx, db, selectedID)
			if err != nil {
				t.Fatal(err)
			}
			after := observations[0]
			// The next read must identify a review with no attempt, which drives the
			// dashboard's Review pending display rather than borrowing implement data.
			if after.Phase != workflow.Review || after.State != workflow.Active || after.Attempt != 0 || after.PRNumber != 42 || after.Agent != "codex" || after.SessionID != "review-session" || after.Model != "" || after.Effort != "" || after.SkillsJSON != "" || after.ProcessSession != "" || after.LogPath != "" {
				t.Fatalf("review pending inherited implement execution: %+v", after)
			}
		})
	}
}
