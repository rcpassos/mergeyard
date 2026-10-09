package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/store"
)

func TestFreshDatabaseSchemaAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state ?#.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	want := map[string][]string{
		"runs":           {"id", "repository", "issue_number", "state", "current_phase", "review_round", "branch", "worktree_path", "pr_number", "implementer_agent", "implementer_session_id", "reviewer_agent", "reviewer_session_id", "approved_sha", "created_at", "updated_at", "completed_at", "last_error_code", "last_error_message", "stop_requested", "takeover_status"},
		"phase_attempts": {"id", "run_id", "phase", "role", "round", "attempt", "agent", "model", "effort", "status", "resumed_session", "process_session", "input_path", "result_path", "log_path", "exit_code", "started_at", "ended_at", "error", "skills_json", "permissions", "session_id"},
		"credit_probes":  {"id", "harness", "restriction_id", "run_id", "attempt_id", "check_id", "status", "requested_at", "ended_at"},
		"harness_limits": {"harness_type", "limited_until", "reset_time_source", "detected_at", "phase_attempt_id", "signal_source", "notified_until", "reason", "restriction_id", "probe_id"},
		"harness_waits":  {"id", "run_id", "phase_attempt_id", "snapshot_json"},
		"events":         {"id", "run_id", "type", "payload_json", "created_at"},
	}
	for table, columns := range want {
		rows, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info(?) ORDER BY cid", table)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			got = append(got, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		if !reflect.DeepEqual(got, columns) {
			t.Errorf("%s columns = %v, want %v", table, got, columns)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO runs (id, repository, issue_number, state) VALUES ('run-1', 'owner/repo', 4, 'CLAIMING')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var state string
	if err := db.QueryRowContext(ctx, "SELECT state FROM runs WHERE id = 'run-1'").Scan(&state); err != nil || state != "CLAIMING" {
		t.Fatalf("restart lost the run: %q, %v", state, err)
	}
}

func TestCanceledOpenPreservesCodePathAndCause(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	path := filepath.Join(t.TempDir(), "state.db")
	_, err := store.Open(ctx, path)
	var failure *fault.Error
	if !errors.As(err, &failure) || failure.Code != "workspace.database_migrate" || failure.Path != path {
		t.Fatalf("expected structured database failure, got %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation cause: %v", err)
	}
}

func TestOnlyOneNonTerminalRunPerIssue(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	insert := func(id, repo string, issue int, state string) error {
		_, err := db.ExecContext(ctx, "INSERT INTO runs (id, repository, issue_number, state) VALUES (?, ?, ?, ?)", id, repo, issue, state)
		return err
	}
	states := []string{"CLAIMING", "PREPARING", "ACTIVE", "WAITING_FOR_CI", "WAITING_FOR_HARNESS", "READY_TO_MERGE", "MANUAL", "NEEDS_ATTENTION"}
	for issue, state := range states {
		if err := insert(state, "owner/repo", issue, state); err != nil {
			t.Fatal(err)
		}
		if err := insert(state+"-duplicate", "owner/repo", issue, "CLAIMING"); err == nil {
			t.Errorf("allowed duplicate non-terminal run for %s", state)
		}
	}
	for _, state := range []string{"FAILED", "STOPPED", "COMPLETED"} {
		if err := insert(state, "owner/repo", 0, state); err != nil {
			t.Fatalf("terminal history must coexist with active run: %v", err)
		}
		if _, err := db.ExecContext(ctx, "UPDATE runs SET state = 'ACTIVE' WHERE id = ?", state); err == nil {
			t.Errorf("allowed terminal run %s to reactivate alongside an active run", state)
		}
	}
	if err := insert("other-repo", "owner/other", 0, "ACTIVE"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE runs SET state = 'COMPLETED' WHERE id = 'CLAIMING'"); err != nil {
		t.Fatal(err)
	}
	if err := insert("retry", "owner/repo", 0, "CLAIMING"); err != nil {
		t.Fatalf("terminal transition did not release issue: %v", err)
	}
}

func TestRelatedRecordsAndHarnessLimitUniqueness(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	queries := []string{
		`INSERT INTO runs (id, repository, issue_number, state) VALUES ('run', 'owner/repo', 4, 'ACTIVE')`,
		`INSERT INTO phase_attempts (id, run_id, phase, role, round, attempt, agent, status) VALUES ('attempt', 'run', 'implement', 'implementer', 0, 1, 'codex', 'running')`,
		`INSERT INTO harness_limits (harness_type, limited_until, reset_time_source, phase_attempt_id) VALUES ('codex', '2026-10-05T00:00:00Z', 'reported', 'attempt')`,
		`INSERT INTO events (run_id, type, payload_json) VALUES ('run', 'phase.started', '{"phase":"implement"}')`,
	}
	for _, query := range queries {
		if _, err := db.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO harness_limits (harness_type, limited_until, reset_time_source) VALUES ('codex', '2026-10-06T00:00:00Z', 'default_cooldown')`); err == nil {
		t.Fatal("allowed two limits for the same harness")
	}
	// Force a new pooled connection to verify foreign keys are configured per connection.
	db.SetMaxIdleConns(0)
	for _, query := range []string{
		`INSERT INTO events (run_id, type, payload_json) VALUES ('missing', 'phase.started', '{}')`,
		`UPDATE phase_attempts SET run_id = 'missing' WHERE id = 'attempt'`,
		`UPDATE harness_limits SET phase_attempt_id = 'missing' WHERE harness_type = 'codex'`,
	} {
		if _, err := db.ExecContext(ctx, query); err == nil {
			t.Errorf("allowed dangling reference: %s", query)
		}
	}
}
