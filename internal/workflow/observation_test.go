package workflow_test

import (
	"context"
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
