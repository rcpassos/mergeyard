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

func TestHarnessCheckUpgradePreservesActiveProbeAndSharedOwnership(t *testing.T) {
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
		if name >= "019_" {
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
		`INSERT INTO runs(id,repository,issue_number,state,current_phase) VALUES('run','owner/repo',7,'ACTIVE','implement')`,
		`INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status) VALUES('execution','run','implement','implementer',0,2,'claude','running')`,
		`INSERT INTO credit_probes(id,harness,restriction_id,run_id,attempt_id,status) VALUES('probe','claude','restriction','run','execution','running')`,
		`INSERT INTO harness_limits(harness_type,reason,restriction_id,probe_id) VALUES('claude','credits_exhausted','restriction','probe')`,
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
	probes, err := workflow.LoadCreditProbes(ctx, db, "run")
	if err != nil || len(probes) != 1 || probes[0].ID != "probe" || probes[0].Status != "running" || probes[0].AttemptID != "execution" {
		t.Fatalf("lost active probe: %+v %v", probes, err)
	}
	limit, err := workflow.LoadHarnessLimit(ctx, db, "claude")
	if err != nil || limit.ProbeAttemptID != "execution" || limit.RestrictionID != "restriction" {
		t.Fatalf("lost restriction: %+v %v", limit, err)
	}
	if _, err := db.Exec(`INSERT INTO harness_checks(id,harness,restriction_id,request_json,process_session,phase_dir,status) VALUES('check','claude','restriction','{}','session','dir','reserved')`); err != nil {
		t.Fatal(err)
	}
	query := `INSERT INTO credit_probes(id,harness,restriction_id,check_id,status) VALUES('check','claude','restriction','check','reserved')`
	if _, err := db.Exec(query); err == nil {
		t.Fatal("check and run owned two probes")
	}
	if _, err := db.Exec(`UPDATE credit_probes SET status='released' WHERE id='probe'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(query); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE credit_probes SET attempt_id='missing' WHERE id='probe'`); err == nil {
		t.Fatal("lost execution foreign key")
	}
	if _, err := db.Exec(`UPDATE credit_probes SET check_id='missing' WHERE id='check'`); err == nil {
		t.Fatal("lost check foreign key")
	}
}
