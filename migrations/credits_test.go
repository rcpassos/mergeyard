package migrations_test

import (
	"context"
	"database/sql"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/rcpassos/mergeyard/internal/workflow"
	"github.com/rcpassos/mergeyard/migrations"
)

func TestCreditRecoveryUpgradePreservesRuntimeAndTimedRestrictions(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		t.Fatal(err)
	}
	previous := fstest.MapFS{}
	names, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name >= "018_" {
			continue
		}
		data, err := fs.ReadFile(migrations.Files, name)
		if err != nil {
			t.Fatal(err)
		}
		previous[name] = &fstest.MapFile{Data: data}
	}
	ctx := context.Background()
	if err := migrations.Apply(ctx, db, previous); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO runs(id,repository,issue_number,state,current_phase,implementer_session_id) VALUES('run','owner/repo',7,'WAITING_FOR_HARNESS','implement','saved-session')`,
		`INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status) VALUES('attempt','run','implement','implementer',0,1,'claude','usage_limited')`,
		`INSERT INTO harness_limits(harness_type,limited_until,reset_time_source,phase_attempt_id,signal_source,notified_until) VALUES('claude','2026-10-09T12:00:00Z','reported','attempt','observed','2026-10-09T12:00:00Z')`,
		`INSERT INTO events(run_id,type,payload_json) VALUES('run','harness.usage_limited','{"original":true}')`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := migrations.Apply(ctx, db, migrations.Files); err != nil {
			t.Fatal(err)
		}
	}
	limit, err := workflow.LoadHarnessLimit(ctx, db, "claude")
	if err != nil || limit == nil || limit.Reason != "temporary_limit" || limit.ResetAt.Format("2006-01-02T15:04:05Z") != "2026-10-09T12:00:00Z" || limit.Source != "observed" || limit.ResetTimeSource != "reported" {
		t.Fatalf("lost timed restriction: %+v %v", limit, err)
	}
	var session, event string
	if err := db.QueryRow(`SELECT implementer_session_id FROM runs WHERE id='run'`).Scan(&session); err != nil || session != "saved-session" {
		t.Fatalf("lost session %s %v", session, err)
	}
	if err := db.QueryRow(`SELECT payload_json FROM events WHERE run_id='run'`).Scan(&event); err != nil || event != `{"original":true}` {
		t.Fatalf("lost event %s %v", event, err)
	}
	if _, err := db.Exec(`UPDATE harness_limits SET phase_attempt_id='missing'`); err == nil {
		t.Fatal("upgrade lost attempt foreign key")
	}
	if _, err := db.Exec(`INSERT INTO credit_probes(id,harness,restriction_id,run_id,status) VALUES('first','claude','restriction','run','reserved')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO credit_probes(id,harness,restriction_id,run_id,status) VALUES('second','claude','restriction','run','reserved')`); err == nil {
		t.Fatal("two durable probe owners accepted")
	}
	if _, err := db.Exec(`UPDATE credit_probes SET status='released' WHERE id='first'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO credit_probes(id,harness,restriction_id,run_id,status) VALUES('second','claude','restriction','run','reserved')`); err != nil {
		t.Fatal(err)
	}
}
