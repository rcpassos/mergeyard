package migrations_test

import (
	"context"
	"testing"

	"github.com/rcpassos/mergeyard/migrations"
)

func TestPublicationRetriesBackfill(t *testing.T) {
	db := migratedBefore(t, "030_publication_retries.sql")
	defer db.Close()
	ctx := context.Background()
	_, err := db.ExecContext(ctx, `
INSERT INTO runs(id,repository,issue_number,state) VALUES('backfill','owner/repo',7,'MANUAL');
INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status,step) VALUES
 ('forbidden','backfill','review','reviewer',1,1,'claude','succeeded',''),
 ('timeout','backfill','fix','implementer',1,1,'claude','succeeded','');
INSERT INTO report_publications(attempt_id,run_id,repository,pr_number,phase,round,attempt,body,warning_code) VALUES
 ('forbidden','backfill','owner/repo',101,'review',1,1,'body','github.forbidden'),
 ('timeout','backfill','owner/repo',101,'fix',1,1,'body','github.timeout');
INSERT INTO events(id,run_id,type,payload_json) VALUES
 (1,'backfill','run.needs_attention','{"run":{}}'),
 (2,'backfill','run.needs_attention','{"run":{}}');
INSERT INTO attention_publications(event_id,run_id,repository,issue_number,body,warning_code) VALUES
 (1,'backfill','owner/repo',7,'body','github.invalid_input'),
 (2,'backfill','owner/repo',7,'body','publication.failed');`)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrations.Apply(ctx, db, migrations.Files); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, table, key, id string
		failed               bool
	}{
		{"permanent report", "report_publications", "attempt_id", "forbidden", true},
		{"expired report", "report_publications", "attempt_id", "timeout", false},
		{"permanent attention", "attention_publications", "event_id", "1", true},
		{"expired attention", "attention_publications", "event_id", "2", false},
	} {
		var failed, stopped bool
		query := "SELECT failed,stopped FROM " + tc.table + " WHERE " + tc.key + "=?"
		if err := db.QueryRow(query, tc.id).Scan(&failed, &stopped); err != nil || failed != tc.failed || stopped {
			t.Fatalf("%s = failed %v stopped %v, %v", tc.name, failed, stopped, err)
		}
	}
}
