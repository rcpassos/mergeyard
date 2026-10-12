package workflow_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/store"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestHumanActionSurvivesRestartAndClearsWithFault(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	w := workflow.New(db, events.New(db, slog.New(slog.NewTextHandler(io.Discard, nil))))
	run := seed(t, w, workflow.Active, workflow.Implement)
	action := &fault.HumanAction{Decision: "Reject duplicates or overwrite?", Files: []string{"internal/import.go"}, Options: []string{"Reject", "Overwrite"}}
	_, err = w.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.PhaseBlocked, Failure: &fault.Error{Code: "phase.blocked", Message: "Need input", Err: &fault.Error{Code: "phase.blocked", HumanAction: action}}})
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
	defer db.Close()
	w = workflow.New(db, events.New(db, slog.New(slog.NewTextHandler(io.Discard, nil))))
	run, err = w.Get(ctx, run.ID)
	if err != nil || !reflect.DeepEqual(run.HumanAction, action) {
		t.Fatalf("action=%+v err=%v", run.HumanAction, err)
	}
	run, err = w.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.Retry, NextState: workflow.Active, NextPhase: workflow.Implement})
	if err != nil {
		t.Fatal(err)
	}
	if run.HumanAction != nil || run.LastErrorCode != "" || run.LastErrorMessage != "" {
		t.Fatalf("stale fault: %+v", run)
	}
}

func TestEveryAttentionTransitionGeneratesAction(t *testing.T) {
	for _, code := range []string{"git.no_changes", "git.base_conflict", "review.max_rounds_exceeded", "phase.result_invalid", "phase.blocked", "workspace.setup_failed", "checks.failed", "harness.usage_limit_waits_exhausted", "pr.closed_unmerged", "ci.check_failed", "future.adapter_error"} {
		t.Run(code, func(t *testing.T) {
			w, _, _ := newWorkflow(t)
			run := seed(t, w, workflow.Active, workflow.Implement)
			run, err := w.Transition(context.Background(), run.ID, workflow.Request{Trigger: workflow.OperationFailed, Failure: &fault.Error{Code: code, Message: "Reported condition"}})
			if err != nil || run.HumanAction == nil || strings.TrimSpace(run.HumanAction.Decision) == "" || run.HumanAction.Files == nil || run.HumanAction.Options == nil {
				t.Fatalf("no action: %+v %v", run, err)
			}
		})
	}
}
