package durable_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/execution/durable"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestLaunchRejectsMismatchedPreparedIntent(t *testing.T) {
	for _, kind := range []workflow.Phase{workflow.Implement, workflow.Review} {
		for _, change := range []struct {
			name   string
			mutate func(*runner.SessionRequest)
		}{
			{"run", func(r *runner.SessionRequest) { r.RunID = "other-run" }},
			{"phase", func(r *runner.SessionRequest) { r.Phase = "other-phase" }},
			{"round", func(r *runner.SessionRequest) { r.Round++ }},
			{"directory", func(r *runner.SessionRequest) { r.PhaseDir = filepath.Join(r.PhaseDir, "other") }},
			{"attempt", func(r *runner.SessionRequest) { r.Attempt++ }},
		} {
			t.Run(string(kind)+"/"+change.name, func(t *testing.T) {
				e, _, run, root := executor(t)
				run.ReviewRound = 2
				ctx := context.Background()
				phase, previous, err := e.Prepare(ctx, root, run, kind, config.Role{Agent: "claude"}, 1)
				if err != nil {
					t.Fatal(err)
				}
				round := 0
				if kind == workflow.Review {
					round = run.ReviewRound
				}
				req := runner.SessionRequest{RunID: run.ID, Phase: string(kind), Round: round, Attempt: 1, PhaseDir: phase.PhaseDir}
				change.mutate(&req)
				e.Runner = &rejectRunner{Runner: e.Runner, before: func() { t.Fatal("mismatched process launched") }}
				history, err := e.Events.History(ctx, 0, 100)
				if err != nil {
					t.Fatal(err)
				}
				a, err := e.Launch(ctx, run, config.Role{Agent: "claude"}, durable.Permissions{}, phase, previous, req, filepath.Join(phase.PhaseDir, "input.md"), "", func(*sql.Tx, durable.Attempt) error {
					t.Fatal("mismatched attempt reached transaction hook")
					return nil
				}, nil)
				var coded *fault.Error
				if !errors.As(err, &coded) || coded.Code != "phase.invalid_request" || a.ID != "" {
					t.Fatalf("mismatch accepted: %+v %v", a, err)
				}
				if _, err := e.Load(ctx, run.ID, kind, round); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("mismatch saved attempt: %v", err)
				}
				after, err := e.Events.History(ctx, 0, 100)
				if err != nil || len(after) != len(history) {
					t.Fatalf("mismatch published events: %+v %v", after, err)
				}
			})
		}
	}
}
