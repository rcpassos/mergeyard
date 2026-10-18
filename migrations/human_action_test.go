package migrations_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/store"
	"github.com/rcpassos/mergeyard/migrations"
)

func TestHumanActionUpgradesPublishedIssueRefusalSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer func() {
		if db != nil {
			db.Close()
		}
	}()
	// Main's published schema ends at issue refusals (22). Build it from the
	// actual migration bytes, excluding this PR's unpublished migration.
	published := fstest.MapFS{}
	names, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name > "021_checks.sql" && name != "022_issue_refusals.sql" {
			continue
		}
		data, err := fs.ReadFile(migrations.Files, name)
		if err != nil {
			t.Fatal(err)
		}
		published[name] = &fstest.MapFile{Data: data}
	}
	if err := migrations.Apply(ctx, db, published); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO issue_refusals(repository,issue_number,reason,ready_label) VALUES('owner/repo',7,'untrusted actor','ready-for-agent')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO runs(id,repository,issue_number,state,last_error_code,last_error_message) VALUES('upgrade-run','owner/repo',8,'NEEDS_ATTENTION','git.no_changes','No changes to publish')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(ctx, path)
	if err != nil {
		t.Fatalf("main schema 22 cannot start after upgrade: %v", err)
	}
	var version, pending int
	var reason, raw string
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 29 {
		t.Fatalf("schema = %d, err %v; want 29", version, err)
	}
	if err := db.QueryRow("SELECT reason,pending FROM issue_refusals WHERE issue_number=7").Scan(&reason, &pending); err != nil || reason != "untrusted actor" || pending != 1 {
		t.Fatalf("refusal lost: %q %d %v", reason, pending, err)
	}
	if err := db.QueryRow("SELECT human_action_json FROM runs WHERE id='upgrade-run'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var action fault.HumanAction
	if err := json.Unmarshal([]byte(raw), &action); err != nil {
		t.Fatal(err)
	}
	if err := action.Validate("blocked"); err != nil || !strings.Contains(action.Decision, "mergeyard retry upgrade-run") {
		t.Fatalf("attention not backfilled: %+v %v", action, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(ctx, path)
	if err != nil {
		t.Fatalf("upgraded database cannot restart: %v", err)
	}
	var saved string
	if err := db.QueryRow("SELECT human_action_json FROM runs WHERE id='upgrade-run'").Scan(&saved); err != nil || saved != raw {
		t.Fatalf("restart changed action: %q %v", saved, err)
	}
}
