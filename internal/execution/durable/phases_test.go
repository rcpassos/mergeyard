package durable_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/execution/durable"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func launchPhase(t *testing.T, e durable.Executor, run workflow.Run, root, agent string, number int, kind workflow.Phase) (durable.Attempt, error) {
	t.Helper()
	ctx := context.Background()
	phase, previous, err := e.Prepare(ctx, root, run, kind, config.Role{Agent: agent}, number)
	if err != nil {
		return durable.Attempt{}, err
	}
	return e.Launch(ctx, run, config.Role{Agent: agent, Model: "saved-model", Effort: "high", Skills: []string{"review"}}, harness.Permissions{Summary: "saved permissions", Mode: "saved-mode", AllowedTools: []string{"Read"}}, phase, previous, runner.SessionRequest{RunID: run.ID, Phase: string(kind), Round: run.ReviewRound, Attempt: number, PhaseDir: phase.PhaseDir}, filepath.Join(phase.PhaseDir, "input.md"), "", func(tx *sql.Tx, a durable.Attempt) error {
		if kind == workflow.Fix {
			_, err := tx.ExecContext(ctx, `INSERT INTO fix_attempts(attempt_id,target_sha,findings_json,session_id,permission_mode,allowed_tools_json,ci_json) VALUES (?,'pinned-head','[]',?,'read-write','[]','{"repair_cause":"ci.check_failed"}')`, a.ID, a.SessionID)
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO review_attempts(attempt_id,target_sha,diff,snapshot_json,session_id,permission_mode,allowed_tools_json) VALUES (?,'pinned-head','saved diff','{}',?,'read-only','[]')`, a.ID, a.SessionID)
		return err
	}, map[string]any{"target_sha": "pinned-head"})
}

func assertPhaseLaunchMetadata(t *testing.T, e durable.Executor, a durable.Attempt, kind workflow.Phase, round int, previousSession, warning string) {
	t.Helper()
	history, err := e.Events.History(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	latest := history[len(history)-1]
	if latest.Type != "phase.attempt_started" {
		t.Fatalf("latest event: %s", latest.Type)
	}
	var payload map[string]any
	if err := json.Unmarshal(latest.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"agent": a.Agent, "phase": string(kind), "round": float64(round),
		"attempt": float64(a.Number), "resumed_session": a.Resumed,
		"session_id": a.SessionID, "previous_session_id": previousSession,
		"warning_code": warning, "check_retry_of": "",
		"model": "saved-model", "effort": "high", "skills": []any{"review"},
		"permissions": "saved permissions", "permission_mode": "saved-mode", "allowed_tools": []any{"Read"},
		"target_sha": "pinned-head",
	} {
		if !reflect.DeepEqual(payload[key], want) {
			t.Errorf("launch metadata %s = %#v; want %#v", key, payload[key], want)
		}
	}
}

func TestPhaseLaunchCommitsFactsProbeRecoveryAndEventTogether(t *testing.T) {
	for _, kind := range []workflow.Phase{workflow.Review, workflow.Fix} {
		for _, agent := range []string{"claude", "codex"} {
			t.Run(string(kind)+"/"+agent, func(t *testing.T) {
				e, w, run, root := executor(t)
				run.ReviewRound = 2
				ctx := context.Background()
				// Replace a lost role conversation once without changing the other role.
				otherRole := workflow.Implement.Role()
				if kind == workflow.Fix {
					otherRole = workflow.Review.Role()
				}
				_, err := e.DB.ExecContext(ctx, "UPDATE runs SET "+otherRole+"_session_id='independent',"+kind.Role()+"_session_id='lost-session',"+kind.Role()+"_agent=?,review_round=2 WHERE id=?", agent, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				e.Runner = &rejectRunner{Runner: e.Runner}
				first, err := launchPhase(t, e, run, root, agent, 1, kind)
				if err == nil || !first.Resumed {
					t.Fatalf("first: %+v %v", first, err)
				}
				assertPhaseLaunchMetadata(t, e, first, kind, run.ReviewRound, "lost-session", "")
				for _, statement := range []string{
					`INSERT INTO harness_limits(harness_type,reason,restriction_id,probe_id) VALUES('` + agent + `','credits_exhausted','restriction','probe')`,
					`INSERT INTO credit_probes(id,harness,restriction_id,run_id,status) VALUES('probe','` + agent + `','restriction','run-141','reserved')`,
					`CREATE TRIGGER reject_phase_launch BEFORE INSERT ON events WHEN NEW.type='phase.attempt_started' BEGIN SELECT RAISE(ABORT,'event unavailable'); END`,
				} {
					if _, err := e.DB.ExecContext(ctx, statement); err != nil {
						t.Fatal(err)
					}
				}
				e.Runner = &rejectRunner{Runner: e.Runner, before: func() { t.Fatal("launched before atomic commit") }}
				second, err := launchPhase(t, e, run, root, agent, 2, kind)
				if err == nil || second.ID != "" {
					t.Fatalf("rollback: %+v %v", second, err)
				}
				saved, err := e.Load(ctx, run.ID, kind, run.ReviewRound)
				if err != nil || saved.ID != first.ID {
					t.Fatalf("rolled back attempt: %+v %v", saved, err)
				}
				var facts int
				if err := e.DB.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM %s_attempts", kind)).Scan(&facts); err != nil || facts != 1 {
					t.Fatalf("rolled back phase facts: %d %v", facts, err)
				}
				current, err := w.Get(ctx, run.ID)
				if err != nil || len(current.SessionRecoveries) != 0 || len(current.CreditProbes) != 1 || current.CreditProbes[0].Status != "reserved" {
					t.Fatalf("rolled back recovery/probe: %+v %v", current, err)
				}
				if _, err := e.DB.ExecContext(ctx, "DROP TRIGGER reject_phase_launch"); err != nil {
					t.Fatal(err)
				}
				e.Runner = &rejectRunner{Runner: e.Runner, before: func() {
					current, err := w.Get(ctx, run.ID)
					if err != nil || len(current.SessionRecoveries) != 1 || current.CreditProbes[0].Status != "running" {
						t.Fatalf("launch facts: %+v %v", current, err)
					}
					saved, err := e.Load(ctx, run.ID, kind, run.ReviewRound)
					if err != nil || saved.ID == first.ID || saved.SessionID == "lost-session" || saved.Resumed {
						t.Fatalf("replacement: %+v %v", saved, err)
					}
					assertPhaseLaunchMetadata(t, e, saved, kind, run.ReviewRound, "lost-session", "harness.session_resume_failed")
					var target string
					if err := e.DB.QueryRowContext(ctx, fmt.Sprintf("SELECT target_sha FROM %s_attempts WHERE attempt_id=?", kind), saved.ID).Scan(&target); err != nil || target != "pinned-head" {
						t.Fatalf("phase facts before startup: %s %v", target, err)
					}
					history, err := e.Events.History(ctx, 0, 100)
					if err != nil || history[len(history)-1].Type != "phase.attempt_started" || history[len(history)-2].Type != "harness.session_resume_failed" {
						t.Fatalf("events: %+v %v", history, err)
					}
				}}
				second, err = launchPhase(t, e, run, root, agent, 2, kind)
				if err == nil || second.ID == "" {
					t.Fatalf("definite failure: %+v %v", second, err)
				}
				var independent, target string
				if err := e.DB.QueryRowContext(ctx, "SELECT "+otherRole+"_session_id FROM runs WHERE id=?", run.ID).Scan(&independent); err != nil || independent != "independent" {
					t.Fatalf("independent role: %s %v", independent, err)
				}
				if err := e.DB.QueryRowContext(ctx, fmt.Sprintf("SELECT target_sha FROM %s_attempts WHERE attempt_id=?", kind), second.ID).Scan(&target); err != nil || target != "pinned-head" {
					t.Fatalf("phase facts: %s %v", target, err)
				}
				if kind == workflow.Fix {
					var findings, diagnostics string
					if err := e.DB.QueryRowContext(ctx, "SELECT findings_json,ci_json FROM fix_attempts WHERE attempt_id=?", second.ID).Scan(&findings, &diagnostics); err != nil || findings != "[]" || diagnostics != `{"repair_cause":"ci.check_failed"}` {
						t.Fatalf("fix facts: %s %s %v", findings, diagnostics, err)
					}
				}
				e.Runner = &rejectRunner{Runner: e.Runner, before: func() { t.Fatal("reused recovery reservation") }}
				third, err := launchPhase(t, e, run, root, agent, 3, kind)
				if err == nil || third.ID != "" {
					t.Fatalf("duplicate recovery: %+v %v", third, err)
				}
			})
		}
	}
}

type phaseProcessRunner struct {
	runner.Runner
	reads  []string
	starts int
}

func (r *phaseProcessRunner) StartSession(context.Context, runner.SessionRequest) (runner.SessionRef, error) {
	r.starts++
	return runner.SessionRef{Name: "ambiguous-phase"}, errors.New("lost launch response")
}
func (r *phaseProcessRunner) SessionStatus(context.Context, runner.SessionRef) (runner.SessionStatus, error) {
	code := 0
	return runner.SessionStatus{State: runner.SessionExited, ExitCode: &code}, nil
}
func (r *phaseProcessRunner) ReadFile(_ context.Context, path string) ([]byte, error) {
	r.reads = append(r.reads, filepath.Base(path))
	if filepath.Base(path) == "events.jsonl" {
		return []byte(`{"type":"thread.started","thread_id":"01a10c5b-38a9-7330-b833-04e365cb5f37"}` + "\n"), nil
	}
	return nil, nil
}

func TestPhaseRestartObservation(t *testing.T) {
	for _, kind := range []workflow.Phase{workflow.Review, workflow.Fix} {
		for _, agent := range []string{"claude", "codex"} {
			t.Run(string(kind)+"/"+agent, func(t *testing.T) {
				e, _, run, root := executor(t)
				run.ReviewRound = 1
				if kind == workflow.Fix {
					if _, err := e.DB.Exec("UPDATE runs SET implementer_session_id='01a10c5b-38a9-7330-b833-04e365cb5f37',implementer_agent=? WHERE id=?", agent, run.ID); err != nil {
						t.Fatal(err)
					}
				}
				r := &phaseProcessRunner{Runner: e.Runner}
				e.Runner = r
				a, err := launchPhase(t, e, run, root, agent, 1, kind)
				if err != nil || a.Status != "running" {
					t.Fatalf("ambiguous launch: %+v %v", a, err)
				}
				previousSession := ""
				if a.Resumed {
					previousSession = a.SessionID
				}
				assertPhaseLaunchMetadata(t, e, a, kind, run.ReviewRound, previousSession, "")
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
				saved, err := e.Load(context.Background(), run.ID, kind, run.ReviewRound)
				if err != nil || saved.ID != a.ID {
					t.Fatalf("restart: %+v %v", saved, err)
				}
				o, err := e.ObserveProcess(context.Background(), run.ID, kind, &saved)
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
				if r.starts != 1 {
					t.Fatalf("ambiguous launch replayed: %d", r.starts)
				}
				// The native identity is captured without launching another attempt.
				if agent == "codex" && saved.SessionID == "" {
					t.Fatal("role identity not discovered")
				}
			})
		}
	}
}

func TestFixRequiresOriginalConversationAndLoadsLegacyAttemptIdentity(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			e, _, run, root := executor(t)
			ctx := context.Background()
			run.ReviewRound = 1
			_, _, err := e.Prepare(ctx, root, run, workflow.Fix, config.Role{Agent: agent}, 1)
			var coded *fault.Error
			if !errors.As(err, &coded) || coded.Code != "harness.session_missing" {
				t.Fatalf("fix without original conversation: %v", err)
			}
			if _, err := e.DB.ExecContext(ctx, "UPDATE runs SET implementer_session_id='original',implementer_agent=? WHERE id=?", agent, run.ID); err != nil {
				t.Fatal(err)
			}
			e.Runner = &phaseProcessRunner{Runner: e.Runner}
			a, err := launchPhase(t, e, run, root, agent, 1, workflow.Fix)
			if err != nil || !a.Resumed || a.SessionID != "original" {
				t.Fatalf("fix resume: %+v %v", a, err)
			}
			// Before migration, the fix identity was stored only in fix_attempts.
			// A later run identity must not replace that historical conversation.
			if _, err := e.DB.ExecContext(ctx, "UPDATE phase_attempts SET session_id=NULL WHERE id=?", a.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := e.DB.ExecContext(ctx, "UPDATE runs SET implementer_session_id='replacement' WHERE id=?", run.ID); err != nil {
				t.Fatal(err)
			}
			saved, err := e.Load(ctx, run.ID, workflow.Fix, run.ReviewRound)
			if err != nil || saved.ID != a.ID || saved.SessionID != "original" {
				t.Fatalf("legacy identity: %+v %v", saved, err)
			}
		})
	}
}
