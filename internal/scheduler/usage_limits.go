package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
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
	if s.harnesses[agent].NativeSucceeded(artifacts) {
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
		limit, err := workflow.LoadHarnessLimit(ctx, s.db, name)
		if err != nil {
			return nil, err
		}
		if limit != nil {
			v.ResetAt, v.ResetTimeSource, v.Source = limit.ResetAt, limit.ResetTimeSource, limit.Source
			v.Available = !s.deps.Now().Before(limit.ResetAt)
		}
		result = append(result, v)
	}
	return result, nil
}

func (s *Scheduler) harnessGate(ctx context.Context, agent string) (*workflow.HarnessWait, error) {
	limit, err := workflow.LoadHarnessLimit(ctx, s.db, agent)
	if err != nil {
		return nil, err
	}
	if limit == nil || !s.deps.Now().Before(limit.ResetAt) {
		return nil, nil
	}
	return &workflow.HarnessWait{Harness: agent, Reason: "temporary_limit", ResetAt: limit.ResetAt, ResetTimeSource: limit.ResetTimeSource, Source: limit.Source, DetectedAt: s.deps.Now().UTC()}, nil
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

// Describe only the execution actually being resumed. An ordinary retry or a
// fresh missing-session recovery has its own context, not an old limit notice.
func interruptionContext(previous attempt, phase harness.PhaseContext) string {
	if previous.status != "usage_limited" || !phase.Resume || previous.sessionID == "" || previous.sessionID != phase.SessionID {
		return ""
	}
	work := "Partial edits are preserved; inspect them before continuing."
	if phase.Phase == workflow.Review {
		work = "Reviewer changes from that execution were restored; review the pinned target again without editing repository files."
	}
	return fmt.Sprintf("The previous %s execution (%s) was interrupted by a temporary usage limit. Resume this phase in the same conversation. %s Effective settings and full phase context are reapplied.", phase.Phase, previous.id, work)
}
