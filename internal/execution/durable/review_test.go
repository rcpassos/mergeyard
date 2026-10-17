package durable_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/execution/durable"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func launchReview(t *testing.T, e durable.Executor, run workflow.Run, root, agent string, number int) (durable.Attempt, error) {
	t.Helper()
	ctx := context.Background()
	phase, previous, err := e.Prepare(ctx, root, run, workflow.Review, config.Role{Agent: agent}, number)
	if err != nil {
		return durable.Attempt{}, err
	}
	return e.Launch(ctx, run, config.Role{Agent: agent, Model: "review-model", Effort: "high", Skills: []string{"review"}}, "review permissions", phase, previous, runner.SessionRequest{RunID: run.ID, Phase: "review", Round: run.ReviewRound, Attempt: number, PhaseDir: phase.PhaseDir}, filepath.Join(phase.PhaseDir, "input.md"), "", func(tx *sql.Tx, a durable.Attempt) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO review_attempts(attempt_id,target_sha,diff,snapshot_json,session_id,permission_mode,allowed_tools_json) VALUES (?,'pinned-head','saved diff','{}',?,'read-only','[]')`, a.ID, a.SessionID)
		return err
	}, map[string]any{"target_sha": "pinned-head"})
}

func TestReviewLaunchCommitsFactsProbeRecoveryAndEventTogether(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			e, w, run, root := executor(t)
			run.ReviewRound = 2
			ctx := context.Background()
			// Fixture a previously resumed reviewer whose conversation was lost. The
			// executor must replace it once, while leaving the implementer independent.
			_, err := e.DB.ExecContext(ctx, `UPDATE runs SET implementer_session_id='implementer',reviewer_session_id='lost-reviewer',reviewer_agent=?,review_round=2 WHERE id=?`, agent, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			e.Runner = &rejectRunner{Runner: e.Runner}
			first, err := launchReview(t, e, run, root, agent, 1)
			if err == nil || !first.Resumed {
				t.Fatalf("first: %+v %v", first, err)
			}
			for _, statement := range []string{
				`INSERT INTO harness_limits(harness_type,reason,restriction_id,probe_id) VALUES('` + agent + `','credits_exhausted','restriction','probe')`,
				`INSERT INTO credit_probes(id,harness,restriction_id,run_id,status) VALUES('probe','` + agent + `','restriction','run-141','reserved')`,
				`CREATE TRIGGER reject_review_launch BEFORE INSERT ON events WHEN NEW.type='phase.attempt_started' BEGIN SELECT RAISE(ABORT,'event unavailable'); END`,
			} {
				if _, err := e.DB.ExecContext(ctx, statement); err != nil {
					t.Fatal(err)
				}
			}
			e.Runner = &rejectRunner{Runner: e.Runner, before: func() { t.Fatal("launched before atomic commit") }}
			second, err := launchReview(t, e, run, root, agent, 2)
			if err == nil || second.ID != "" {
				t.Fatalf("rollback: %+v %v", second, err)
			}
			saved, err := e.Load(ctx, run.ID, workflow.Review, run.ReviewRound)
			if err != nil || saved.ID != first.ID {
				t.Fatalf("rolled back attempt: %+v %v", saved, err)
			}
			current, err := w.Get(ctx, run.ID)
			if err != nil || len(current.SessionRecoveries) != 0 || len(current.CreditProbes) != 1 || current.CreditProbes[0].Status != "reserved" {
				t.Fatalf("rolled back recovery/probe: %+v %v", current, err)
			}
			if _, err := e.DB.ExecContext(ctx, "DROP TRIGGER reject_review_launch"); err != nil {
				t.Fatal(err)
			}
			e.Runner = &rejectRunner{Runner: e.Runner, before: func() {
				current, err := w.Get(ctx, run.ID)
				if err != nil || len(current.SessionRecoveries) != 1 || current.CreditProbes[0].Status != "running" {
					t.Fatalf("launch facts: %+v %v", current, err)
				}
				saved, err := e.Load(ctx, run.ID, workflow.Review, run.ReviewRound)
				if err != nil || saved.ID == first.ID || saved.SessionID == "lost-reviewer" || saved.Resumed {
					t.Fatalf("replacement: %+v %v", saved, err)
				}
				history, err := e.Events.History(ctx, 0, 100)
				if err != nil || history[len(history)-1].Type != "phase.attempt_started" || history[len(history)-2].Type != "harness.session_resume_failed" {
					t.Fatalf("events: %+v %v", history, err)
				}
			}}
			second, err = launchReview(t, e, run, root, agent, 2)
			if err == nil || second.ID == "" {
				t.Fatalf("definite failure: %+v %v", second, err)
			}
			var implementer, target string
			if err := e.DB.QueryRowContext(ctx, "SELECT implementer_session_id FROM runs WHERE id=?", run.ID).Scan(&implementer); err != nil || implementer != "implementer" {
				t.Fatalf("implementer: %s %v", implementer, err)
			}
			if err := e.DB.QueryRowContext(ctx, "SELECT target_sha FROM review_attempts WHERE attempt_id=?", second.ID).Scan(&target); err != nil || target != "pinned-head" {
				t.Fatalf("review facts: %s %v", target, err)
			}
			e.Runner = &rejectRunner{Runner: e.Runner, before: func() { t.Fatal("reused recovery reservation") }}
			third, err := launchReview(t, e, run, root, agent, 3)
			if err == nil || third.ID != "" {
				t.Fatalf("duplicate recovery: %+v %v", third, err)
			}
		})
	}
}

type reviewProcessRunner struct {
	runner.Runner
	reads []string
}

func (r *reviewProcessRunner) StartSession(context.Context, runner.SessionRequest) (runner.SessionRef, error) {
	return runner.SessionRef{Name: "ambiguous-review"}, errors.New("lost launch response")
}
func (r *reviewProcessRunner) SessionStatus(context.Context, runner.SessionRef) (runner.SessionStatus, error) {
	code := 0
	return runner.SessionStatus{State: runner.SessionExited, ExitCode: &code}, nil
}
func (r *reviewProcessRunner) ReadFile(_ context.Context, path string) ([]byte, error) {
	r.reads = append(r.reads, filepath.Base(path))
	if filepath.Base(path) == "events.jsonl" {
		return []byte(`{"type":"thread.started","thread_id":"01a10c5b-38a9-7330-b833-04e365cb5f37"}` + "\n"), nil
	}
	return nil, nil
}

func TestReviewRestartObservationDefersArtifactsUntilRestoration(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			e, _, run, root := executor(t)
			run.ReviewRound = 1
			r := &reviewProcessRunner{Runner: e.Runner}
			e.Runner = r
			a, err := launchReview(t, e, run, root, agent, 1)
			if err != nil || a.Status != "running" {
				t.Fatalf("ambiguous launch: %+v %v", a, err)
			}
			if err := e.DB.Close(); err != nil {
				t.Fatal(err)
			}
			e.DB, err = openAgain(root)
			if err != nil {
				t.Fatal(err)
			}
			defer e.DB.Close()
			// Rebuild the event bus as well, so captured Codex identity is saved after restart.
			e.Events = events.New(e.DB, nil)
			saved, err := e.Load(context.Background(), run.ID, workflow.Review, run.ReviewRound)
			if err != nil || saved.ID != a.ID {
				t.Fatalf("restart: %+v %v", saved, err)
			}
			o, err := e.ObserveProcess(context.Background(), run.ID, workflow.Review, &saved)
			if err != nil || o.Failure != nil {
				t.Fatalf("process: %+v %v", o, err)
			}
			for _, name := range r.reads {
				if name != "events.jsonl" {
					t.Fatalf("final artifacts read before caller restoration: %v", r.reads)
				}
			}
			o = e.ReadArtifacts(context.Background(), saved, o)
			if o.Failure != nil || o.Artifacts.ExitCode != 0 || len(o.Artifacts.Stdout) == 0 {
				t.Fatalf("artifacts: %+v", o)
			}
			// The native identity is captured without launching another attempt.
			if agent == "codex" && saved.SessionID == "" {
				t.Fatal("review identity not discovered")
			}
		})
	}
}
