package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

const waitsExhausted = "harness.usage_limit_waits_exhausted"

type temporaryLimitError struct {
	outcome harness.FailureClassification
	exit    int
}

func (*temporaryLimitError) Error() string {
	return "harness.temporary_limit: Execution interrupted by a temporary usage limit"
}

// Parse semantic results first. Valid blocked/failed task reports and successful
// native completion never enter the classifier, even when tool text names limits.
func (s *Scheduler) parseExecution(agent string, phase harness.PhaseContext, artifacts harness.PhaseArtifacts) (harness.PhaseResult, error) {
	result, err := s.harnesses[agent].ParseResult(phase, artifacts)
	if err == nil {
		return result, nil
	}
	code := errorCodeForReview(err)
	nativeFailure := artifacts.ExitCode != 0 || code == "phase.execution_failed" || code == "harness.session_resume_failed" || code == "phase.max_turns_exceeded" || code == "phase.budget_exceeded"
	if !nativeFailure {
		return result, err
	}
	outcome := s.harnesses[agent].ClassifyFailure(artifacts, s.deps.Now().UTC())
	if outcome.Kind == harness.TemporaryLimit {
		return result, &temporaryLimitError{outcome: outcome, exit: artifacts.ExitCode}
	}
	// Credits are a distinct classified outcome; recovery is implemented in #83.
	if outcome.Kind == harness.CreditsExhausted {
		return result, &fault.Error{Code: "harness.credits_exhausted", Message: "Harness credits are exhausted; explicit recovery is required"}
	}
	return result, err
}

func isTemporaryLimit(err error) bool { var limit *temporaryLimitError; return errors.As(err, &limit) }

func (s *Scheduler) limitExecution(ctx context.Context, repo config.Repository, run workflow.Run, a attempt, cause error) error {
	var limit *temporaryLimitError
	if !errors.As(cause, &limit) {
		return cause
	}
	now := s.deps.Now().UTC()
	wait := workflow.HarnessWait{Harness: a.agent, Reason: "temporary_limit", Phase: run.Phase, Round: phaseRound(run, run.Phase), AttemptID: a.id, Attempt: a.number, ExitCode: &limit.exit, DetectedAt: now, ResetAt: now.Add(s.cfg.UsageLimits.Cooldown), ResetTimeSource: "default_cooldown", Source: limit.outcome.Source, Allowance: s.cfg.UsageLimits.MaxWaits}
	if limit.outcome.ResetAt.After(now) {
		wait.ResetAt = limit.outcome.ResetAt.UTC()
		wait.ResetTimeSource = "reported"
	}
	var lastOrdinary int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(max(attempt),0) FROM phase_attempts WHERE run_id=? AND phase=? AND round=? AND status NOT IN ('running','usage_limited')`, run.ID, run.Phase, wait.Round).Scan(&lastOrdinary); err != nil {
		return err
	}
	wait.Sequence = lastOrdinary + 1
	if err := s.db.QueryRowContext(ctx, `SELECT count(*)+1 FROM phase_attempts WHERE run_id=? AND phase=? AND round=? AND status='usage_limited' AND attempt>?`, run.ID, run.Phase, wait.Round, lastOrdinary).Scan(&wait.Consecutive); err != nil {
		return err
	}
	for _, v := range run.Retries {
		if !v.Pending && v.Error == "" && v.NextPhase == run.Phase && v.Round == run.ReviewRound && v.WaitSequence == wait.Sequence && v.GrantedWait > wait.Allowance {
			wait.Allowance = v.GrantedWait
		}
	}
	trigger := workflow.HarnessLimited
	var failure *fault.Error
	if wait.Consecutive >= wait.Allowance {
		trigger = workflow.OperationFailed
		failure = &fault.Error{Code: waitsExhausted, Message: "Consecutive usage-limit waits exhausted; Retry grants exactly one additional wait and respects the known reset time"}
	}
	_, err := s.workflow.Transition(ctx, run.ID, workflow.Request{Trigger: trigger, Failure: failure, Metadata: workflow.MetadataPatch{HarnessWait: &wait}})
	if err != nil {
		return err
	}
	if err := s.updateHarnessAvailability(ctx); err != nil {
		return err
	}
	if failure != nil {
		return s.attentionLabels(ctx, repo, run)
	}
	return nil
}

// HarnessAvailability is shared across repositories and survives restart.
type HarnessAvailability struct {
	Harness                   string    `json:"harness"`
	Available                 bool      `json:"available"`
	ResetAt                   time.Time `json:"reset_at,omitempty"`
	ResetTimeSource           string    `json:"reset_time_source,omitempty"`
	Source                    string    `json:"source,omitempty"`
	TemporaryLimitDetection   bool      `json:"temporary_limit_detection"`
	CreditExhaustionDetection bool      `json:"credit_exhaustion_detection"`
}

func (s *Scheduler) HarnessAvailability(ctx context.Context) ([]HarnessAvailability, error) {
	var result []HarnessAvailability
	for _, name := range []string{"claude", "codex"} {
		caps := s.harnesses[name].Capabilities()
		v := HarnessAvailability{Harness: name, Available: true, TemporaryLimitDetection: caps.TemporaryLimitDetection, CreditExhaustionDetection: caps.CreditExhaustionDetection}
		var reset string
		err := s.db.QueryRowContext(ctx, "SELECT limited_until,reset_time_source,signal_source FROM harness_limits WHERE harness_type=?", name).Scan(&reset, &v.ResetTimeSource, &v.Source)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			v.ResetAt, err = time.Parse(time.RFC3339Nano, reset)
			if err != nil {
				return nil, err
			}
			v.Available = !s.deps.Now().Before(v.ResetAt)
		}
		result = append(result, v)
	}
	return result, nil
}

func (s *Scheduler) harnessGate(ctx context.Context, agent string) (*workflow.HarnessWait, error) {
	var reset, source, signal string
	err := s.db.QueryRowContext(ctx, "SELECT limited_until,reset_time_source,signal_source FROM harness_limits WHERE harness_type=?", agent).Scan(&reset, &source, &signal)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	at, err := time.Parse(time.RFC3339Nano, reset)
	if err != nil {
		return nil, err
	}
	if !s.deps.Now().Before(at) {
		return nil, nil
	}
	return &workflow.HarnessWait{Harness: agent, Reason: "temporary_limit", ResetAt: at, ResetTimeSource: source, Source: signal, DetectedAt: s.deps.Now().UTC()}, nil
}

func (s *Scheduler) gatePhase(ctx context.Context, run workflow.Run, agent string) (bool, error) {
	wait, err := s.harnessGate(ctx, agent)
	if err != nil {
		return false, err
	}
	if wait == nil {
		return false, nil
	}
	wait.Phase, wait.Round = run.Phase, phaseRound(run, run.Phase)
	_, err = s.workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.HarnessLimited, Metadata: workflow.MetadataPatch{HarnessWait: wait}})
	return true, err
}

func (s *Scheduler) resumeHarnessWait(ctx context.Context, run workflow.Run) error {
	wait := run.HarnessWait
	if wait == nil || wait.Phase != run.Phase || wait.Round != phaseRound(run, run.Phase) {
		return &fault.Error{Code: "harness.wait_missing", Message: "Harness wait metadata is unavailable; inspect preserved work"}
	}
	gate, err := s.harnessGate(ctx, wait.Harness)
	if err != nil {
		return err
	}
	if gate != nil || s.deps.Now().Before(wait.ResetAt) {
		return nil
	}
	_, err = s.workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.HarnessAvailable})
	return err // A later tick launches the same phase, with a distinct execution.
}

// Persist notification acknowledgement with each availability event. Recovery
// can replay this after a crash without repeating an unchanged harness event.
func (s *Scheduler) updateHarnessAvailability(ctx context.Context) error {
	for _, name := range []string{"claude", "codex"} {
		var reset, notified, source, signal string
		err := s.db.QueryRowContext(ctx, "SELECT limited_until,notified_until,reset_time_source,signal_source FROM harness_limits WHERE harness_type=?", name).Scan(&reset, &notified, &source, &signal)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if reset != notified {
			_, err = s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
				_, err := tx.ExecContext(ctx, "UPDATE harness_limits SET notified_until=limited_until WHERE harness_type=?", name)
				return events.Draft{Type: "harness.usage_limited", Payload: map[string]string{"harness": name, "reset_at": reset, "reset_time_source": source, "source": signal}}, err
			})
			if err != nil {
				return err
			}
		}
		at, err := time.Parse(time.RFC3339Nano, reset)
		if err != nil {
			return err
		}
		if !s.deps.Now().Before(at) {
			_, err = s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
				_, err := tx.ExecContext(ctx, "DELETE FROM harness_limits WHERE harness_type=? AND limited_until=?", name, reset)
				return events.Draft{Type: "harness.available", Payload: map[string]string{"harness": name}}, err
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func interruptionContext(run workflow.Run, phase workflow.Phase) string {
	for i := len(run.HarnessWaitHistory) - 1; i >= 0; i-- {
		wait := run.HarnessWaitHistory[i]
		if wait.Phase == phase && wait.Round == phaseRound(run, phase) && wait.AttemptID != "" {
			return fmt.Sprintf("The previous %s execution was interrupted by a temporary usage limit. Resume the same conversation and phase. Partial implementer edits are preserved; inspect them before continuing. Interrupted reviewer changes were restored. Settings and full phase context are reapplied. Interrupted execution: %s; reset source: %s.", phase, wait.AttemptID, wait.ResetTimeSource)
		}
	}
	return ""
}
