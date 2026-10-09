package migrations_test

import (
	"context"
	"database/sql"
	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/migrations"
	"io/fs"
	"testing"
	"testing/fstest"
)

func TestCIUpgradePreservesPublishedM2History(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	published := fstest.MapFS{}
	names := []string{"001_initial.sql", "002_scheduler.sql", "003_stop_requested.sql", "004_attempt_skills.sql", "005_reviews.sql", "006_fixes.sql", "007_implement_settings.sql", "008_effective_codex_permissions.sql", "009_report_publications.sql"}
	for _, name := range names {
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
	if _, err := db.Exec(`INSERT INTO runs(id,repository,issue_number,state,current_phase,review_round,pr_number,approved_sha) VALUES('m2','owner/repo',7,'WAITING_FOR_CI','review',2,101,'approved');
 INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status,permissions) VALUES('implement','m2','implement','implementer',0,1,'codex','succeeded','workspace-write · network true · approvals never');
 INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status) VALUES('fix','m2','fix','implementer',1,1,'claude','succeeded');
 INSERT INTO fix_attempts(attempt_id,session_id,target_sha,findings_json,permission_mode,allowed_tools_json,commit_sha,pushed) VALUES('fix','session','old','[]','auto','[]','approved',1);
 INSERT INTO events(run_id,type,payload_json) VALUES('m2','fix.pushed','{"commit_sha":"approved"}');
 INSERT INTO report_publications(attempt_id,run_id,repository,pr_number,phase,round,attempt,body,state,comment_id,comment_url) VALUES('fix','m2','owner/repo',101,'fix',1,1,'frozen body','published',42,'https://github.com/owner/repo/pull/101#issuecomment-42');`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrations.Apply(ctx, db, migrations.Files); err != nil {
			t.Fatal(err)
		}
	}
	var state, sha, commit, event, permissions string
	var round, version int
	if err := db.QueryRow("SELECT state,approved_sha,review_round FROM runs WHERE id='m2'").Scan(&state, &sha, &round); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT commit_sha FROM fix_attempts WHERE attempt_id='fix'").Scan(&commit); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT payload_json FROM events WHERE run_id='m2'").Scan(&event); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT permissions FROM phase_attempts WHERE id='implement'").Scan(&permissions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	var publicationBody, publicationState string
	var commentID int
	if err := db.QueryRow("SELECT body,state,comment_id FROM report_publications WHERE attempt_id='fix'").Scan(&publicationBody, &publicationState, &commentID); err != nil {
		t.Fatal(err)
	}
	wait, err := ci.Load(ctx, db, "m2")
	if err != nil || wait != nil || state != "WAITING_FOR_CI" || sha != "approved" || round != 2 || commit != "approved" || event != `{"commit_sha":"approved"}` || permissions != "workspace-write · network true · approvals never" || version != 19 || publicationBody != "frozen body" || publicationState != "published" || commentID != 42 {
		t.Fatal("CI upgrade changed existing M2 history")
	}
}
