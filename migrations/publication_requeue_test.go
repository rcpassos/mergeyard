package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/migrations"
)

// A publication stopped by a repeated failure records a stop event; one
// stopped by its first permanent failure records a warning event only.
func TestPublicationRequeueRecoversTheStoppedReason(t *testing.T) {
	db := migratedBefore(t, "031_publication_requeue.sql")
	defer db.Close()
	ctx := context.Background()
	// The runtime enforces foreign keys, and the upgrade rebuilds a table.
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys(1)`); err != nil {
		t.Fatal(err)
	}
	_, err := db.ExecContext(ctx, `
INSERT INTO runs(id,repository,issue_number,state) VALUES('stopped','owner/repo',7,'MANUAL');
INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status,step) VALUES
  ('attempt','stopped','review','reviewer',1,1,'claude','succeeded','');
INSERT INTO report_publications(attempt_id,run_id,repository,pr_number,phase,round,attempt,body,warning_code,warning,stopped,failed) VALUES
  ('attempt','stopped','owner/repo',101,'review',1,1,'body','github.forbidden','Report is saved locally. PR comment publication cannot deliver this comment.',1,1);
INSERT INTO events(id,run_id,type,payload_json) VALUES
  (1,'stopped','run.needs_attention','{"run":{}}'),
  (2,'stopped','publication.stopped','{"attention_event_id":1,"code":"github.forbidden","message":"Human action is saved locally. Attention comment publication cannot deliver this comment."}');
INSERT INTO attention_publications(event_id,run_id,repository,issue_number,body,warning_code,stopped,failed) VALUES
  (1,'stopped','owner/repo',7,'body','github.forbidden',1,1);
INSERT INTO events(id,run_id,type,payload_json) VALUES
  (3,'stopped','publication.stopped','{"source_attempt":"attempt","code":"github.not_found","message":"Verification evidence is saved locally. PR body publication cannot update this PR."}');
INSERT INTO verification_attempts(run_id,source_attempt,attempts,last_attempt_at,failed,stopped) VALUES
  ('stopped','attempt',1,'2026-01-01T00:00:00.000Z',1,1);`)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrations.Apply(ctx, db, migrations.Files); err != nil {
			t.Fatal(err)
		}
	}
	var attention, verification, code string
	if err := db.QueryRow(`SELECT warning,warning_code FROM attention_publications WHERE run_id='stopped'`).Scan(&attention, &code); err != nil {
		t.Fatal(err)
	}
	if code != "github.forbidden" || attention == "" {
		t.Fatalf("attention reason = %q %q", code, attention)
	}
	if err := db.QueryRow(`SELECT warning,warning_code FROM verification_attempts WHERE run_id='stopped'`).Scan(&verification, &code); err != nil {
		t.Fatal(err)
	}
	if code != "github.not_found" || verification == "" {
		t.Fatalf("verification reason = %q %q", code, verification)
	}
}

// The first permanent failure of a publication records a warning event, and
// the stopped row is never attempted again, so no stop event follows it.
func TestPublicationRequeueRecoversTheFirstFailureReason(t *testing.T) {
	db := migratedBefore(t, "031_publication_requeue.sql")
	defer db.Close()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys(1)`); err != nil {
		t.Fatal(err)
	}
	_, err := db.ExecContext(ctx, `
INSERT INTO runs(id,repository,issue_number,state) VALUES('first','owner/repo',7,'MANUAL');
INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status,step) VALUES
  ('attempt','first','implement','implementer',0,1,'claude','succeeded','');
INSERT INTO events(id,run_id,type,payload_json) VALUES
  (1,'first','run.needs_attention','{"run":{}}'),
  (2,'first','publication.warning','{"attention_event_id":1,"code":"github.forbidden","message":"Human action is saved locally. Attention comment publication cannot deliver this comment."}'),
  (3,'first','verification.warning','{"source_attempt":"attempt","code":"github.not_found","message":"Verification evidence is saved locally. PR body publication cannot update this PR."}');
INSERT INTO attention_publications(event_id,run_id,repository,issue_number,body,warning_code,stopped,failed) VALUES
  (1,'first','owner/repo',7,'body','github.forbidden',1,1);
INSERT INTO verification_attempts(run_id,source_attempt,attempts,last_attempt_at,failed,stopped) VALUES
  ('first','attempt',1,'2026-01-01T00:00:00.000Z',1,1);`)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrations.Apply(ctx, db, migrations.Files); err != nil {
			t.Fatal(err)
		}
	}
	var attention, attentionCode, verification, verificationCode string
	if err := db.QueryRow(`SELECT warning,warning_code FROM attention_publications WHERE run_id='first'`).Scan(&attention, &attentionCode); err != nil {
		t.Fatal(err)
	}
	if attentionCode != "github.forbidden" || !strings.Contains(attention, "cannot deliver") {
		t.Fatalf("attention reason = %q %q", attentionCode, attention)
	}
	if err := db.QueryRow(`SELECT warning,warning_code FROM verification_attempts WHERE run_id='first'`).Scan(&verification, &verificationCode); err != nil {
		t.Fatal(err)
	}
	if verificationCode != "github.not_found" || !strings.Contains(verification, "cannot update this PR") {
		t.Fatalf("verification reason = %q %q", verificationCode, verification)
	}
}
