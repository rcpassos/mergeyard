// Package durable owns the journal and process lifecycle of coding-agent attempts.
// Phase input, retry authorization, workflow decisions and publication remain
// with the phase's caller.
package durable

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/sessions"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// Executor uses the existing harness and runner boundaries, including after restart.
type Executor struct {
	DB        *sql.DB
	Workflow  *workflow.Workflow
	Events    *events.Bus
	Runner    runner.Runner
	Harnesses map[string]harness.HarnessAdapter
	Now       func() time.Time
}

type Attempt struct {
	ID, Agent             string
	Number                int
	Status                string
	Ref                   runner.SessionRef
	SessionID             string
	Resumed               bool
	Failure, CheckRetryOf string
}

func (e Executor) LoadImplement(ctx context.Context, id string) (Attempt, error) {
	var a Attempt
	var input string
	var saved sql.NullString
	err := e.DB.QueryRowContext(ctx, `SELECT id,attempt,status,COALESCE(process_session,''),COALESCE(input_path,''),resumed_session,COALESCE(error,''),agent,session_id,check_retry_of FROM phase_attempts WHERE run_id=? AND phase='implement' AND step='' ORDER BY attempt DESC LIMIT 1`, id).Scan(&a.ID, &a.Number, &a.Status, &a.Ref.Name, &input, &a.Resumed, &a.Failure, &a.Agent, &saved, &a.CheckRetryOf)
	if err != nil {
		return a, err
	}
	a.Ref.PhaseDir = filepath.Dir(input)
	err = e.DB.QueryRowContext(ctx, "SELECT COALESCE(implementer_session_id,'') FROM runs WHERE id=?", id).Scan(&a.SessionID)
	if saved.Valid {
		a.SessionID = saved.String
	}
	return a, err
}

// PrepareImplement establishes identity and a required process journal before
// the phase writes input or builds its real harness invocation.
func (e Executor) PrepareImplement(ctx context.Context, root string, run workflow.Run, role config.Role, number int) (harness.PhaseContext, Attempt, error) {
	var phase harness.PhaseContext
	var previous Attempt
	var sessionID, agent string
	if err := e.DB.QueryRowContext(ctx, "SELECT COALESCE(implementer_session_id,''),COALESCE(implementer_agent,'') FROM runs WHERE id=?", run.ID).Scan(&sessionID, &agent); err != nil {
		return phase, previous, err
	}
	if agent != "" && agent != role.Agent {
		return phase, previous, &fault.Error{Code: "harness.session_agent_changed", Message: "Persisted implementer belongs to " + agent + "; restore that role harness before resuming"}
	}
	resume := sessionID != ""
	if number > 1 {
		var err error
		previous, err = e.LoadImplement(ctx, run.ID)
		if err != nil {
			return phase, previous, err
		}
		if previous.Status == "usage_limited" && sessionID == "" {
			return phase, previous, &fault.Error{Code: "harness.session_missing", Message: "Interrupted conversation identity is unavailable; inspect preserved work"}
		}
		if strings.HasPrefix(previous.Failure, "harness.session_resume_failed: ") {
			sessionID, resume = "", false
		}
	}
	if e.Harnesses[role.Agent].Capabilities().SessionIDSource == harness.Preassigned && sessionID == "" {
		sessionID = uuid.NewString()
	}
	dir := filepath.Join(root, "runs", run.ID, "phases", fmt.Sprintf("implement-0-%d", number))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return phase, previous, err
	}
	if err := sessions.RequireProcessJournal(dir); err != nil {
		return phase, previous, err
	}
	return harness.PhaseContext{Phase: workflow.Implement, PhaseDir: dir, SessionID: sessionID, Resume: resume}, previous, nil
}

// LaunchImplement commits all executable intent before invoking the runner.
// An ambiguous launch remains running and is observed through its saved ref.
// A returned attempt with an error denotes a definite, durably failed startup.
func (e Executor) LaunchImplement(ctx context.Context, run workflow.Run, role config.Role, permissions string, phase harness.PhaseContext, previous Attempt, req runner.SessionRequest, input, checkID string) (Attempt, error) {
	skills, err := json.Marshal(role.Skills)
	if err != nil {
		return Attempt{}, err
	}
	a := Attempt{ID: uuid.NewString(), Agent: role.Agent, Number: req.Attempt, Status: "running", Ref: runner.SessionRef{Name: sessions.Name(req), PhaseDir: req.PhaseDir}, SessionID: phase.SessionID, Resumed: phase.Resume, CheckRetryOf: checkID}
	_, err = e.Events.CommitBatch(ctx, func(tx *sql.Tx) ([]events.Draft, error) {
		if _, err := tx.ExecContext(ctx, "UPDATE runs SET implementer_session_id=? WHERE id=?", phase.SessionID, run.ID); err != nil {
			return nil, err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO phase_attempts (id,run_id,phase,role,round,attempt,agent,model,effort,status,resumed_session,process_session,input_path,result_path,log_path,skills_json,permissions,session_id,check_retry_of,started_at) VALUES (?,?,'implement','implementer',0,?,?,?,?,'running',?,?,?,?,?,?,?,?,?,?)`, a.ID, run.ID, a.Number, role.Agent, role.Model, role.Effort, phase.Resume, a.Ref.Name, input, filepath.Join(req.PhaseDir, "result.json"), filepath.Join(req.PhaseDir, "events.jsonl"), string(skills), permissions, phase.SessionID, checkID, e.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			return nil, err
		}
		if err := workflow.BindCreditProbe(ctx, tx, run.ID, role.Agent, a.ID, e.Now().UTC()); err != nil {
			return nil, err
		}
		drafts := []events.Draft{}
		if strings.HasPrefix(previous.Failure, "harness.session_resume_failed: ") {
			recovery, err := ReserveRecovery(ctx, tx, run, workflow.Implement, previous, a.ID, phase.SessionID)
			if err != nil {
				return nil, err
			}
			drafts = append(drafts, recovery)
		}
		drafts = append(drafts, events.Draft{RunID: run.ID, Type: "phase.attempt_started", Payload: map[string]any{"phase": workflow.Implement, "round": 0, "attempt": a.Number, "check_retry_of": checkID, "resumed_session": phase.Resume}})
		return drafts, nil
	})
	if err != nil {
		return Attempt{}, err
	}
	ref, err := e.Runner.StartSession(ctx, req)
	if err != nil && ctx.Err() == nil {
		if ref.Name != "" {
			return a, nil
		}
		if _, saveErr := e.DB.ExecContext(ctx, `UPDATE phase_attempts SET status='failed',ended_at=strftime('%Y-%m-%dT%H:%M:%fZ','now'),error=? WHERE id=?`, err.Error(), a.ID); saveErr != nil {
			return Attempt{}, saveErr
		}
		a.Status = "failed"
	}
	return a, err
}

// ReserveRecovery belongs to the replacement's launch transaction. A crash
// after commit leaves one deterministic identity to observe, never replay.
func ReserveRecovery(ctx context.Context, tx *sql.Tx, run workflow.Run, phase workflow.Phase, previous Attempt, replacement, session string) (events.Draft, error) {
	round := run.ReviewRound
	if phase == workflow.Implement {
		round = 0
	}
	role := phase.Role()
	_, err := tx.ExecContext(ctx, `INSERT INTO session_recoveries(run_id,phase,role,round,failed_attempt_id,replacement_attempt_id,previous_session_id) VALUES (?,?,?,?,?,?,?)`, run.ID, phase, role, round, previous.ID, replacement, previous.SessionID)
	return events.Draft{RunID: run.ID, Type: "harness.session_resume_failed", Payload: map[string]any{"agent": previous.Agent, "phase": phase, "role": role, "round": round, "attempt": previous.Number, "replacement_attempt": previous.Number + 1, "previous_session_id": previous.SessionID, "session_id": session, "error": previous.Failure, "warning": "Saved conversation is missing; continuing once in a fresh session"}}, err
}

// Discover observes captured identity without launching or resuming a process.
func (e Executor) Discover(ctx context.Context, id string, phase workflow.Phase, a *Attempt) error {
	observer, ok := e.Harnesses[a.Agent].(harness.SessionDiscoverer)
	if !ok {
		return nil
	}
	data, err := e.Runner.ReadFile(ctx, filepath.Join(a.Ref.PhaseDir, "events.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	identity, err := observer.DiscoverSession(data)
	if err != nil {
		return err
	}
	if identity == "" {
		return nil
	}
	role, otherRole := phase.Role(), workflow.Review.Role()
	if role == otherRole {
		otherRole = workflow.Implement.Role()
	}
	var other string
	if err := e.DB.QueryRowContext(ctx, "SELECT COALESCE("+otherRole+"_session_id,'') FROM runs WHERE id=?", id).Scan(&other); err != nil {
		return err
	}
	if identity == other {
		return &fault.Error{Code: "harness.session_identity_invalid", Message: "Harness reused the other role's conversation identity"}
	}
	if a.SessionID != "" {
		if identity != a.SessionID {
			return &fault.Error{Code: "harness.session_identity_invalid", Message: "Codex emitted a different identity than the persisted role session"}
		}
		return nil
	}
	_, err = e.Events.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		_, err := tx.ExecContext(ctx, "UPDATE runs SET "+role+"_session_id=?,updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?", identity, id)
		if err == nil {
			_, err = tx.ExecContext(ctx, "UPDATE phase_attempts SET session_id=? WHERE id=?", identity, a.ID)
		}
		table := ""
		if phase == workflow.Review {
			table = "review_attempts"
		}
		if phase == workflow.Fix {
			table = "fix_attempts"
		}
		if err == nil && table != "" {
			_, err = tx.ExecContext(ctx, "UPDATE "+table+" SET session_id=? WHERE attempt_id=?", identity, a.ID)
		}
		return events.Draft{RunID: id, Type: "harness.session_discovered", Payload: map[string]any{"agent": a.Agent, "session_id": identity, "phase": phase, "attempt": a.Number}}, err
	})
	if err == nil {
		a.SessionID = identity
	}
	return err
}

type Observation struct {
	Status    runner.SessionStatus
	Artifacts harness.PhaseArtifacts
	Failure   error
}

// Observe orders identity validation, owned-process stop, and complete artifact
// reads. Failure is a completed execution failure; the returned error is a
// transient observation error and must not consume an attempt.
func (e Executor) Observe(ctx context.Context, runID string, phase workflow.Phase, a *Attempt) (Observation, error) {
	var o Observation
	var err error
	o.Status, err = e.Runner.SessionStatus(ctx, a.Ref)
	if err != nil {
		return o, err
	}
	o.Failure = e.Discover(ctx, runID, phase, a)
	if o.Failure != nil {
		var coded *fault.Error
		if !errors.As(o.Failure, &coded) || (coded.Code != "phase.result_invalid" && coded.Code != "harness.session_identity_invalid" && coded.Code != "harness.session_resume_failed") {
			return o, o.Failure
		}
		if o.Status.State == runner.SessionRunning {
			if err := e.Runner.StopSession(ctx, a.Ref); err != nil {
				return o, err
			}
			o.Status, err = e.Runner.SessionStatus(ctx, a.Ref)
			if err != nil {
				return o, err
			}
		}
	}
	if o.Status.State == runner.SessionRunning || o.Failure != nil {
		return o, nil
	}
	if o.Status.State != runner.SessionExited || o.Status.ExitCode == nil {
		o.Failure = &fault.Error{Code: "phase.session_missing", Message: "Implement session has no exit metadata; inspect before retrying"}
		return o, nil
	}
	o.Artifacts.ExitCode = *o.Status.ExitCode
	o.Artifacts.Stdout, o.Failure = e.Runner.ReadFile(ctx, filepath.Join(a.Ref.PhaseDir, "events.jsonl"))
	if o.Failure == nil {
		o.Artifacts.Stderr, o.Failure = e.Runner.ReadFile(ctx, filepath.Join(a.Ref.PhaseDir, "stderr.log"))
	}
	if o.Failure == nil && a.Agent == "codex" {
		o.Artifacts.LastMessage, o.Failure = e.Runner.ReadFile(ctx, filepath.Join(a.Ref.PhaseDir, "last-message.json"))
		if errors.Is(o.Failure, os.ErrNotExist) {
			o.Failure = nil
		}
	}
	return o, nil
}
