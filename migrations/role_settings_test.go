package migrations_test

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/store"
	"github.com/rcpassos/mergeyard/internal/workflow"
	"github.com/rcpassos/mergeyard/migrations"
)

func TestRoleSettingsUpgradePreservesLegacyRun(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	published := fstest.MapFS{}
	names, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name >= "024_" {
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
	if _, err := db.Exec(`INSERT INTO runs(id,repository,issue_number,state,current_phase,review_round,pr_number,approved_sha) VALUES('legacy','owner/repo',7,'WAITING_FOR_CI','review',2,101,'approved')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	w := workflow.New(db, events.New(db, nil))
	run, err := w.Get(ctx, "legacy")
	if err != nil || run.RoleSettings != nil || run.State != workflow.WaitingForCI || run.Phase != workflow.Review || run.ReviewRound != 2 || run.PRNumber != 101 || run.ApprovedSHA != "approved" {
		t.Fatalf("legacy run lost on upgrade: %+v %v", run, err)
	}
}
