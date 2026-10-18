package scheduler

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/store"
	"github.com/rcpassos/mergeyard/internal/workflow"
	"github.com/rcpassos/mergeyard/internal/workspace"
	"github.com/rcpassos/mergeyard/migrations"
)

// recordingGitHub observes PR body writes without answering them.
type recordingGitHub struct {
	GitHub
	updated  []int
	contents []github.PullRequestContent
}

func (f *recordingGitHub) UpdatePullRequest(_ context.Context, _ string, n int, content github.PullRequestContent) (*github.PullRequest, error) {
	f.updated = append(f.updated, n)
	f.contents = append(f.contents, content)
	return nil, nil
}

// A database written before implement reports moved onto the attempt must
// upgrade without republishing already-delivered evidence, and must publish the
// reports still waiting. The report and its receipts come from migration 029.
func TestUpgradeDoesNotRepublishLegacyImplementEvidence(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
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
		if name >= "029_implement_reports.sql" {
			continue
		}
		data, err := fs.ReadFile(migrations.Files, name)
		if err != nil {
			t.Fatal(err)
		}
		previous[name] = &fstest.MapFile{Data: data}
	}
	if err := migrations.Apply(ctx, db, previous); err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `
INSERT INTO runs(id,repository,issue_number,state,current_phase,pr_number) VALUES
 ('legacy-published','owner/repo',7,'STOPPED','implement',101),
 ('legacy-pending','owner/repo',8,'STOPPED','implement',102);
INSERT INTO scheduler_runs(run_id,issue_json) VALUES
 ('legacy-published','{"number":7,"title":"Published"}'),
 ('legacy-pending','{"number":8,"title":"Pending"}');
INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status,step) VALUES
 ('legacy-published-attempt','legacy-published','implement','implementer',0,1,'claude','succeeded',''),
 ('legacy-pending-attempt','legacy-pending','implement','implementer',0,1,'claude','succeeded','');
INSERT INTO events(id,run_id,type,payload_json) VALUES
 (1,'legacy-published','phase.completed','{"attempt":1,"result":{"schema_version":4,"human_action":null,"status":"success","summary":"Published change","verification":[{"command":"go test","outcome":"passed","excerpt":"ok"}]},"error":""}'),
 (2,'legacy-published','verification.published','{"source_event":1,"pr_number":101}'),
 (3,'legacy-pending','phase.completed','{"attempt":1,"result":{"schema_version":4,"human_action":null,"status":"success","summary":"Pending change","verification":[{"command":"go test","outcome":"passed","excerpt":"ok"}]},"error":""}');`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
	if err != nil {
		t.Fatal(err)
	}
	home, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bus := events.New(db, nil)
	api := &recordingGitHub{}
	s, err := New(cfg, Resources{DB: db, Events: bus, Workflow: workflow.New(db, bus), Workspace: home}, Dependencies{GitHub: api})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.publishVerification(ctx); err != nil {
		t.Fatal(err)
	}
	if len(api.updated) != 1 || api.updated[0] != 102 {
		t.Fatalf("publication answered %v, want only the pending PR 102", api.updated)
	}
	var receipt string
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(json_extract(payload_json,'$.source_attempt'),'') FROM events WHERE run_id='legacy-pending' AND type='verification.published'`).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	if receipt != "legacy-pending-attempt" {
		t.Fatalf("pending receipt = %q, want the attempt that carries the report", receipt)
	}
	if err := s.publishVerification(ctx); err != nil {
		t.Fatal(err)
	}
	if len(api.updated) != 1 {
		t.Fatalf("receipt did not prevent a second delivery: %v", api.updated)
	}
}
