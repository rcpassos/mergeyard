package migrations_test

import (
	"context"
	"database/sql"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/rcpassos/mergeyard/migrations"
)

// Older binaries kept the implement report only inside completion-event
// payloads, with no phase recorded, and answered verification receipts and
// bookkeeping by completion-event ID. Upgrading must move the report onto the
// attempt, freeze its publication eligibility, point the bookkeeping and
// receipts at the attempt, and leave unrelated rows and events alone. Both the
// pre-bookkeeping and the published 028 database shapes must upgrade.
func TestImplementReportsBackfillFromEventsAndRekeyPublicationReceipts(t *testing.T) {
	cases := []struct {
		name        string
		before      string // first migration this database has not applied
		bookkeeping bool   // database already holds the 028 per-run publication row
	}{
		{"pre-bookkeeping database", "028_verification_attempts.sql", false},
		{"published 028 database", "029_implement_reports.sql", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := migratedBefore(t, tc.before)
			defer db.Close()
			ctx := context.Background()
			_, err := db.ExecContext(ctx, `
INSERT INTO runs(id,repository,issue_number,state,current_phase,review_round,pr_number)
 VALUES ('legacy','owner/repo',7,'ACTIVE','implement',0,101);
INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status,step,error) VALUES
 ('legacy-implement-1','legacy','implement','implementer',0,1,'claude','failed','','phase.blocked: Missing dependency'),
 ('legacy-implement-2','legacy','implement','implementer',0,2,'claude','succeeded','',''),
 ('legacy-implement-invalid','legacy','implement','implementer',0,3,'claude','failed','','phase.result_invalid: Result does not match acceptance criteria'),
 ('legacy-implement-unparsed','legacy','implement','implementer',0,4,'claude','failed','','phase.result_invalid: Structured output must be an implement result object'),
 ('legacy-review-1','legacy','review','reviewer',1,1,'claude','succeeded','',''),
 ('legacy-fix-1','legacy','fix','implementer',1,1,'claude','failed','','phase.result_invalid: Fix claims a fixed finding but made no code changes'),
 ('legacy-fix-2','legacy','fix','implementer',1,2,'claude','failed','','review.head_changed: PR head changed during fix'),
 ('legacy-setup','legacy','implement','system',0,1,'claude','succeeded','setup',''),
 ('legacy-running','legacy','implement','implementer',0,5,'claude','running','','');
INSERT INTO fix_attempts(attempt_id,session_id,target_sha,findings_json,permission_mode,allowed_tools_json,report_json) VALUES
 ('legacy-fix-1','session-1','target','[]','default','[]','{"schema_version":4,"status":"success","summary":"Legacy fix","responses":[],"verification":[]}'),
 ('legacy-fix-2','session-2','target','[]','default','[]','{"schema_version":4,"status":"success","summary":"Changed-head fix","responses":[],"verification":[]}');
INSERT INTO events(id,run_id,type,payload_json) VALUES
 (1,'legacy','phase.completed','{"attempt":1,"result":{"schema_version":2,"human_action":null,"status":"blocked","summary":"Legacy implement"},"error":"phase.blocked: Missing dependency"}'),
 (2,'legacy','phase.completed','{"phase":"implement","attempt":2,"result":{"schema_version":4,"human_action":null,"status":"success","summary":"Second implement","verification":[]},"error":""}'),
 (3,'legacy','phase.completed','{"phase":"review","round":1,"attempt":1,"result":{"schema_version":4,"status":"approved","summary":"Unrelated review"},"error":""}'),
 (4,'legacy','phase.completed','{"phase":"fix","round":1,"attempt":1,"result":{"schema_version":4,"status":"success","summary":"Legacy fix","responses":[],"verification":[]},"error":""}'),
 (5,'legacy','verification.published','{"source_event":2,"pr_number":101}'),
 (6,'legacy','verification.attempted','{"source_event":4,"pr_number":101}'),
 (7,'legacy','verification.published','{"source_event":4,"pr_number":101}'),
 (8,'legacy','verification.warning','{"source_event":1,"code":"github.unavailable"}'),
 (9,'legacy','run.claimed','{"marker":"untouched"}'),
 (10,'legacy','phase.failed','{"phase":"fix","round":1,"attempt":1,"result":{"schema_version":4,"status":"success","summary":"Legacy fix","responses":[],"verification":[]},"error":"phase.result_invalid: Fix claims a fixed finding but made no code changes"}'),
 (11,'legacy','phase.failed','{"phase":"implement","attempt":3,"result":{"schema_version":4,"human_action":null,"status":"success","summary":"Invalid implement","verification":[]},"error":"phase.result_invalid: Result does not match acceptance criteria"}'),
 (12,'legacy','phase.failed','{"phase":"implement","attempt":4,"result":{"schema_version":0,"human_action":null,"status":"failed","summary":""},"error":"phase.result_invalid: Structured output must be an implement result object"}'),
 (13,'legacy','phase.failed','{"phase":"fix","round":1,"attempt":2,"result":{"schema_version":4,"status":"success","summary":"Changed-head fix","responses":[],"verification":[]},"error":"review.head_changed: PR head changed during fix"}');`)
			if err != nil {
				t.Fatal(err)
			}
			if tc.bookkeeping {
				if _, err := db.ExecContext(ctx, `INSERT INTO verification_attempts(run_id,source_event,attempts,last_attempt_at,failed) VALUES('legacy',4,3,'2026-01-01T00:00:00.000Z',1)`); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err := migrations.Apply(ctx, db, migrations.Files); err != nil {
					t.Fatal(err)
				}
				checkRecoveredImplementReports(t, db, tc.bookkeeping)
			}
		})
	}
}

// migratedBefore opens an in-memory database at the schema the release before
// the named migration shipped.
func migratedBefore(t *testing.T, first string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	previous := fstest.MapFS{}
	names, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name >= first {
			continue
		}
		data, err := fs.ReadFile(migrations.Files, name)
		if err != nil {
			t.Fatal(err)
		}
		previous[name] = &fstest.MapFile{Data: data}
	}
	if err := migrations.Apply(context.Background(), db, previous); err != nil {
		t.Fatal(err)
	}
	return db
}

func checkRecoveredImplementReports(t *testing.T, db *sql.DB, bookkeeping bool) {
	t.Helper()
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 29 {
		t.Fatalf("schema version = %d, err %v; want 29", version, err)
	}
	reports := []struct {
		id           string
		version      int
		summary      string
		verification string
		fixReport    bool // report lives in fix_attempts, not phase_attempts
	}{
		{"legacy-implement-1", 2, "Legacy implement", "", false},
		{"legacy-implement-2", 4, "Second implement", "array", false},
		// Parsed but rejected: the dashboard always showed it, publication never used it.
		{"legacy-implement-invalid", 4, "Invalid implement", "array", false},
		{"legacy-implement-unparsed", 0, "", "", false},
		{"legacy-review-1", 0, "", "", false},
		{"legacy-fix-1", 0, "", "", true},
		{"legacy-fix-2", 0, "", "", true},
		{"legacy-setup", 0, "", "", false},
		{"legacy-running", 0, "", "", false},
	}
	for _, tc := range reports {
		var gotVersion int
		var summary, verification string
		if err := db.QueryRow(`SELECT COALESCE(json_extract(report_json,'$.schema_version'),0),COALESCE(json_extract(report_json,'$.summary'),''),COALESCE(json_type(report_json,'$.verification'),'') FROM phase_attempts WHERE id=?`, tc.id).Scan(&gotVersion, &summary, &verification); err != nil {
			t.Fatal(err)
		}
		if gotVersion != tc.version || summary != tc.summary || verification != tc.verification {
			t.Fatalf("%s report = v%d %q verification %q, want v%d %q %q", tc.id, gotVersion, summary, verification, tc.version, tc.summary, tc.verification)
		}
		if tc.fixReport {
			var fixReport sql.NullString
			if err := db.QueryRow("SELECT report_json FROM fix_attempts WHERE attempt_id=?", tc.id).Scan(&fixReport); err != nil {
				t.Fatal(err)
			}
			if !fixReport.Valid {
				t.Fatalf("%s lost its fix report", tc.id)
			}
		}
	}
	// Publication eligibility is frozen from the event-era rule: an eligible
	// completion made the report publishable, whatever happened to the attempt
	// afterwards. Reports stored for an ineligible completion stay unpublished.
	eligibility := map[string]int{
		"legacy-implement-1":        1,
		"legacy-implement-2":        1,
		"legacy-implement-invalid":  0,
		"legacy-implement-unparsed": 0,
		"legacy-review-1":           0,
		"legacy-fix-1":              1,
		"legacy-fix-2":              0,
		"legacy-setup":              0,
		"legacy-running":            0,
	}
	for id, want := range eligibility {
		var eligible int
		if err := db.QueryRow("SELECT report_eligible FROM phase_attempts WHERE id=?", id).Scan(&eligible); err != nil {
			t.Fatal(err)
		}
		if eligible != want {
			t.Fatalf("%s report_eligible = %d, want %d", id, eligible, want)
		}
	}
	receipts := []struct {
		event   int64
		attempt string
		source  int64
	}{
		{5, "legacy-implement-2", 2},
		{7, "legacy-fix-1", 4},
		{8, "legacy-implement-1", 1},
		// The retired per-attempt event is dead history and stays untouched.
		{6, "", 4},
	}
	for _, tc := range receipts {
		var attempt string
		var source int64
		if err := db.QueryRow(`SELECT COALESCE(json_extract(payload_json,'$.source_attempt'),''),COALESCE(json_extract(payload_json,'$.source_event'),0) FROM events WHERE id=?`, tc.event).Scan(&attempt, &source); err != nil {
			t.Fatal(err)
		}
		if attempt != tc.attempt || source != tc.source {
			t.Fatalf("event %d receipt = attempt %q event %d, want attempt %q event %d", tc.event, attempt, source, tc.attempt, tc.source)
		}
	}
	var payload string
	if err := db.QueryRow("SELECT payload_json FROM events WHERE id=9").Scan(&payload); err != nil || payload != `{"marker":"untouched"}` {
		t.Fatalf("unrelated event changed: %q, err %v", payload, err)
	}
	var rows int
	if err := db.QueryRow("SELECT count(*) FROM verification_attempts").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if !bookkeeping {
		if rows != 0 {
			t.Fatalf("bookkeeping rows = %d, want none", rows)
		}
		return
	}
	var attempt, lastAttempt string
	var attempts int
	var failed bool
	if err := db.QueryRow("SELECT source_attempt,attempts,last_attempt_at,failed FROM verification_attempts WHERE run_id='legacy'").Scan(&attempt, &attempts, &lastAttempt, &failed); err != nil {
		t.Fatal(err)
	}
	if attempt != "legacy-fix-1" || attempts != 3 || lastAttempt != "2026-01-01T00:00:00.000Z" || !failed {
		t.Fatalf("bookkeeping row = %q %d %q %v, want the fix attempt with its counts", attempt, attempts, lastAttempt, failed)
	}
}
