package migrations_test

import (
	"context"
	"database/sql"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/rcpassos/mergeyard/migrations"
)

func TestDashboardUpgradesPublishedStopSchema(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	// The published version 3 records Stop intent only. Upgrades must append
	// skills storage instead of editing a migration this database already applied.
	published := fstest.MapFS{
		"003_stop_requested.sql": {Data: []byte("ALTER TABLE runs ADD COLUMN stop_requested INTEGER NOT NULL DEFAULT 0 CHECK (stop_requested IN (0, 1));")},
	}
	for _, name := range []string{"001_initial.sql", "002_scheduler.sql"} {
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
	for range 2 {
		if err := migrations.Apply(ctx, db, migrations.Files); err != nil {
			t.Fatal(err)
		}
		var skills sql.NullString
		if err := db.QueryRowContext(ctx, "SELECT skills_json FROM phase_attempts LIMIT 1").Scan(&skills); err != sql.ErrNoRows {
			t.Fatalf("upgraded database cannot read attempt skills: %v", err)
		}
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 16 {
		t.Fatalf("dashboard schema version = %d, err %v; want 16", version, err)
	}
}
