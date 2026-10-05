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

func TestUpgradeCorrectsCodexFullAccessPermissionSnapshots(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	previous := fstest.MapFS{}
	for _, name := range []string{"001_initial.sql", "002_scheduler.sql", "003_stop_requested.sql", "004_attempt_skills.sql", "005_reviews.sql", "006_implement_settings.sql"} {
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
	cases := []struct {
		name, agent string
		permissions any
		expected    string
	}{
		{"full access", "codex", "danger-full-access · network false · approvals never", "danger-full-access · network true · approvals never"},
		{"workspace", "codex", "workspace-write · network false · approvals never", "workspace-write · network false · approvals never"},
		{"claude", "claude", "bypassPermissions · allowed tools ", "bypassPermissions · allowed tools "},
		{"unknown", "codex", nil, "Not recorded for this older attempt"},
	}
	for i, tc := range cases {
		if _, err := db.ExecContext(ctx, "INSERT INTO runs(id,repository,issue_number,state,current_phase) VALUES (?,'owner/repo',?,'ACTIVE','implement')", tc.name, i+1); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status,permissions) VALUES (?,?,'implement','implementer',0,1,?,'succeeded',?)", tc.name, tc.name, tc.agent, tc.permissions); err != nil {
			t.Fatal(err)
		}
	}
	// Upgrade and replay must expose correct historical permissions to every view.
	for range 2 {
		if err := migrations.Apply(ctx, db, migrations.Files); err != nil {
			t.Fatal(err)
		}
		for _, tc := range cases {
			snapshot, err := workflow.LoadImplementSnapshot(ctx, db, tc.name)
			if err != nil || snapshot == nil {
				t.Fatalf("%s snapshot: %+v %v", tc.name, snapshot, err)
			}
			if snapshot.Permissions != tc.expected {
				t.Fatalf("%s permissions = %q, want %q", tc.name, snapshot.Permissions, tc.expected)
			}
		}
	}
}
