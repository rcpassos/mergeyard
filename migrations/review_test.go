package migrations_test

import (
	"context"
	"database/sql"
	"github.com/rcpassos/mergeyard/migrations"
	"io/fs"
	"testing"
	"testing/fstest"
)

func TestReviewsUpgradeM1WithoutRewritingHistory(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	published := fstest.MapFS{}
	for _, name := range []string{"001_initial.sql", "002_scheduler.sql", "003_stop_requested.sql", "004_attempt_skills.sql"} {
		data, err := fs.ReadFile(migrations.Files, name)
		if err != nil {
			t.Fatal(err)
		}
		published[name] = &fstest.MapFile{Data: data}
	}
	ctx := context.Background()
	if err := migrations.Apply(ctx, db, published); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO runs(id,repository,issue_number,state,current_phase,review_round,pr_number) VALUES ('m1','owner/repo',7,'ACTIVE','review',1,101);
 INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status) VALUES ('a','m1','implement','implementer',0,1,'claude','succeeded');
 INSERT INTO events(run_id,type,payload_json) VALUES ('m1','pr.created','{"old":true}');`)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrations.Apply(ctx, db, migrations.Files); err != nil {
			t.Fatal(err)
		}
	}
	var state, phase, event, status string
	var round, pr, count int
	if err := db.QueryRow("SELECT state,current_phase,review_round,pr_number FROM runs WHERE id='m1'").Scan(&state, &phase, &round, &pr); err != nil {
		t.Fatal(err)
	}
	db.QueryRow("SELECT payload_json FROM events WHERE run_id='m1'").Scan(&event)
	db.QueryRow("SELECT status FROM phase_attempts WHERE id='a'").Scan(&status)
	db.QueryRow("SELECT count(*) FROM review_attempts").Scan(&count)
	if state != "ACTIVE" || phase != "review" || round != 1 || pr != 101 || event != `{"old":true}` || status != "succeeded" || count != 0 {
		t.Fatal("migration rewrote M1 history")
	}
}
