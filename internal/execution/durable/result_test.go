package durable_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/execution/durable"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/store"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type classified struct {
	harness.HarnessAdapter
	calls int
}

func (c *classified) ClassifyFailure(harness.PhaseArtifacts, time.Time) harness.FailureClassification {
	c.calls++
	return harness.FailureClassification{Kind: harness.TemporaryLimit, Source: "test"}
}

func executor(t *testing.T) (durable.Executor, *workflow.Workflow, workflow.Run, string) {
	t.Helper()
	root := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	bus := events.New(db, nil)
	w := workflow.New(db, bus)
	run, err := w.Transition(context.Background(), "run-141", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 141})
	if err != nil {
		t.Fatal(err)
	}
	return durable.Executor{DB: db, Events: bus, Workflow: w, Runner: runner.NewLocal(runner.Options{}), Harnesses: map[string]harness.HarnessAdapter{"claude": harness.NewClaude(config.Claude{}), "codex": harness.NewCodex(config.Codex{})}, Now: time.Now}, w, run, root
}

func TestNativeCompletionPrecedesLimitClassification(t *testing.T) {
	e, _, run, _ := executor(t)
	adapter := &classified{HarnessAdapter: e.Harnesses["codex"]}
	e.Harnesses["codex"] = adapter
	phase := harness.PhaseContext{Phase: workflow.Implement}
	// Native completion remains authoritative even with an invalid task report.
	_, err := e.Parse(context.Background(), run.ID, "", "codex", phase, harness.PhaseArtifacts{Stdout: []byte("{\"type\":\"turn.completed\"}\n"), LastMessage: []byte(`{"status":"invalid"}`)})
	var limit *durable.TemporaryLimitError
	if err == nil || errors.As(err, &limit) || adapter.calls != 0 {
		t.Fatalf("completion precedence: err=%v classifications=%d", err, adapter.calls)
	}
	_, err = e.Parse(context.Background(), run.ID, "", "codex", phase, harness.PhaseArtifacts{ExitCode: 1})
	if !errors.As(err, &limit) || adapter.calls != 1 {
		t.Fatalf("incomplete failure: err=%v classifications=%d", err, adapter.calls)
	}
}

func openAgain(root string) (*sql.DB, error) {
	return store.Open(context.Background(), filepath.Join(root, "state.db"))
}
