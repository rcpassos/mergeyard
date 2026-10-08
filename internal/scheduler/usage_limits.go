package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"strings"
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
func (s *Scheduler) parseExecution(ctx context.Context, runID, attemptID, agent string, phase harness.PhaseContext, artifacts harness.PhaseArtifacts) (harness.PhaseResult, error) {
	result, err := s.harnesses[agent].ParseResult(phase, artifacts)
	completed := s.harnesses[agent].NativeSucceeded(artifacts)
	if completed {
		if proofErr := s.workflow.RecordCreditProof(ctx, runID, attemptID, s.deps.Now().UTC()); proofErr != nil {
			return result, proofErr
		}
	}
	if err == nil || completed {
		return result, err
	}
	outcome := s.harnesses[agent].ClassifyFailure(artifacts, s.deps.Now().UTC())
	if outcome.Kind == harness.TemporaryLimit {
		return result, &temporaryLimitError{outcome: outcome, exit: artifacts.ExitCode}
	}
	// The core consumes classified outcomes; native credit signals belong to #83.
	if outcome.Kind == harness.CreditsExhausted {
		return result, &creditLimitError{outcome: outcome, exit: artifacts.ExitCode}
	}
	return result, err
}

type creditLimitError struct {
	outcome harness.FailureClassification
	exit    int
}

func (*creditLimitError) Error() string {
	return "harness.credits_exhausted: Harness credits are exhausted; explicit recovery is required"
}
func isHarnessLimit(err error) bool {
	var temporary *temporaryLimitError
	var credit *creditLimitError
	return errors.As(err, &temporary) || errors.As(err, &credit)
}
func (s *Scheduler) creditExecution(ctx context.Context, repo config.Repository, run workflow.Run, a attempt, cause *creditLimitError) error {
	wait := workflow.HarnessWait{Harness: a.agent, Reason: "credits_exhausted", Phase: run.Phase, Round: phaseRound(run, run.Phase), AttemptID: a.id, Attempt: a.number, ExitCode: &cause.exit, DetectedAt: s.deps.Now().UTC(), Source: cause.outcome.Source}
	_, err := s.workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.OperationFailed, Failure: &fault.Error{Code: "harness.credits_exhausted", Message: "Harness credits are exhausted; restore credits and explicitly Retry to probe recovery"}, Metadata: workflow.MetadataPatch{HarnessWait: &wait}})
	if err != nil {
		return err
	}
	return s.attentionLabels(ctx, repo, run)
}

func (s *Scheduler) limitExecution(ctx context.Context, repo config.Repository, run workflow.Run, a attempt, cause error) error {
	var credit *creditLimitError
	if errors.As(cause, &credit) {
		return s.creditExecution(ctx, repo, run, a, credit)
	}
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
	wait.Consecutive = 1
	// Credit interruptions preserve attempts but do not spend temporary waits.
	for _, previous := range run.HarnessWaitHistory {
		if previous.Reason == "temporary_limit" && previous.Phase == wait.Phase && previous.Round == wait.Round && previous.Attempt > lastOrdinary {
			wait.Consecutive++
		}
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
	Reason                    string    `json:"reason,omitempty"`
	ProbeID                   string    `json:"probe_id,omitempty"`
	ProbeRunID                string    `json:"probe_run_id,omitempty"`
	ProbeAttemptID            string    `json:"probe_attempt_id,omitempty"`
	Harness                   string    `json:"harness"`
	Available                 bool      `json:"available"`
	ResetAt                   time.Time `json:"reset_at,omitzero"`
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
			if limit.Reason == "credits_exhausted" && !s.deps.Now().Before(limit.ResetAt) {
				v.ResetAt = time.Time{}
				v.ResetTimeSource = ""
			}
			v.Reason, v.ProbeID, v.ProbeRunID, v.ProbeAttemptID = limit.Reason, limit.ProbeID, limit.ProbeRunID, limit.ProbeAttemptID
			v.Available = limit.Reason != "credits_exhausted" && !s.deps.Now().Before(limit.ResetAt)
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
	if limit == nil || (limit.Reason != "credits_exhausted" && !s.deps.Now().Before(limit.ResetAt)) {
		return nil, nil
	}
	return &workflow.HarnessWait{Harness: agent, Reason: limit.Reason, ResetAt: limit.ResetAt, ResetTimeSource: limit.ResetTimeSource, Source: limit.Source, DetectedAt: s.deps.Now().UTC()}, nil
}

func (s *Scheduler) gatePhase(ctx context.Context, run workflow.Run, agent string) (bool, error) {
	wait, err := s.harnessGate(ctx, agent)
	if err != nil {
		return false, err
	}
	if wait == nil {
		return false, nil
	}
	if wait.Reason == "credits_exhausted" {
		limit, err := workflow.LoadHarnessLimit(ctx, s.db, agent)
		if err != nil {
			return false, err
		}
		if s.creditProbeReady(limit, run.ID) {
			return false, nil
		}
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
	if gate != nil {
		limit, err := workflow.LoadHarnessLimit(ctx, s.db, wait.Harness)
		if err != nil {
			return err
		}
		if !s.creditProbeReady(limit, run.ID) {
			return nil
		}
	}
	if s.deps.Now().Before(wait.ResetAt) {
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
	reason := "a temporary usage limit"
	if strings.HasPrefix(previous.failure, "harness.credits_exhausted:") {
		reason = "exhausted credits; this execution is the explicitly requested recovery probe"
	}
	return fmt.Sprintf("The previous %s execution (%s) was interrupted by %s. Resume this phase in the same conversation. %s Effective settings and full phase context are reapplied.", phase.Phase, previous.id, reason, work)
}

func (s *Scheduler) selectCreditProbe(ctx context.Context, run workflow.Run, repo config.Repository, v workflow.RetrySnapshot, patch *workflow.MetadataPatch) error {
	starts, err := s.retryStartsAgent(ctx, run, v)
	if err != nil {
		return err
	}
	if !starts {
		return nil
	}
	agent := repo.Implementer.Agent
	if v.NextPhase == workflow.Review {
		agent = repo.Reviewer.Agent
	}
	limit, err := workflow.LoadHarnessLimit(ctx, s.db, agent)
	if err != nil {
		return err
	}
	if limit == nil || limit.Reason != "credits_exhausted" {
		return nil
	}
	if !s.harnesses[agent].Capabilities().CreditExhaustionDetection {
		return &fault.Error{Code: "harness.credit_detection_unavailable", Message: "Credit recovery is unavailable until this harness supports exhausted-credit detection"}
	}
	patch.CreditProbe = &workflow.CreditProbe{ID: uuid.NewString(), Harness: agent, RestrictionID: limit.RestrictionID, RunID: run.ID, Status: "reserved"}
	return nil
}

// RetryEligible keeps credit recovery surfaces behind adapter capability. The
// reconciliation in Retry remains authoritative about the selected next work.
func (s *Scheduler) RetryEligible(ctx context.Context, run workflow.Run) (bool, error) {
	if !run.RetryAllowed() || run.Merge != nil || run.TakeoverStatus == workflow.TakeoverRequested {
		return false, nil
	}
	var stopping bool
	if err := s.db.QueryRowContext(ctx, "SELECT stop_requested FROM runs WHERE id=?", run.ID).Scan(&stopping); err != nil {
		return false, err
	}
	if stopping {
		return false, nil
	}
	for _, p := range run.CreditProbes {
		if p.Status == "reserved" || p.Status == "running" {
			return false, nil
		}
	}
	repo, ok := s.repository(run.Repository)
	if !ok {
		return false, nil
	}
	phase := run.Phase
	state := workflow.Active
	if run.PRNumber > 0 {
		head := ""
		if run.Review != nil {
			head = run.Review.TargetSHA
		}
		if run.Phase == workflow.Fix && run.Fix != nil && run.Fix.Round == run.ReviewRound && run.Fix.Status == "succeeded" && run.Fix.CommitSHA != "" {
			head = run.Fix.CommitSHA
		}
		if run.CI != nil && run.CI.SHA == head && run.CI.CurrentHead != "" {
			head = run.CI.CurrentHead
		}
		approved := run.ApprovedSHA != "" && run.ApprovedSHA == head && run.Review != nil && run.Review.Accepted && run.Review.Report != nil && run.Review.Report.Status == "approved" && run.Review.TargetSHA == head
		state, phase = retryPRWork(run, head, approved)
	}
	starts, err := s.retryStartsAgent(ctx, run, workflow.RetrySnapshot{NextState: state, NextPhase: phase})
	if err != nil {
		return false, err
	}
	if !starts {
		return true, nil
	}
	agent := repo.Implementer.Agent
	if phase == workflow.Review {
		agent = repo.Reviewer.Agent
	}
	limit, err := workflow.LoadHarnessLimit(ctx, s.db, agent)
	if err != nil {
		return false, err
	}
	if limit != nil && limit.Reason == "credits_exhausted" {
		return limit.ProbeID == "" && s.harnesses[agent].Capabilities().CreditExhaustionDetection, nil
	}
	return true, nil
}

func (s *Scheduler) creditProbeReady(limit *workflow.HarnessLimit, runID string) bool {
	return limit != nil && limit.Reason == "credits_exhausted" && limit.ProbeRunID == runID && limit.ProbeAttemptID == "" && !s.deps.Now().Before(limit.ResetAt) && s.harnesses[limit.Harness].Capabilities().CreditExhaustionDetection
}

// Publication and saved fix completion reuse evidence; they cannot prove credit
// recovery and must never reserve the account's only model probe.
func (s *Scheduler) retryStartsAgent(ctx context.Context, run workflow.Run, v workflow.RetrySnapshot) (bool, error) {
	if v.NextState != workflow.Active {
		return false, nil
	}
	if v.AttemptFrom > 0 {
		return true, nil
	}
	var a attempt
	var err error
	switch v.NextPhase {
	case workflow.Implement:
		a, err = s.lastAttempt(ctx, run.ID)
	case workflow.Fix:
		var f fixAttempt
		f, err = s.lastFix(ctx, run)
		a = f.attempt
	default:
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	return a.status != "succeeded", err
}

// runSnapshot keeps current credit-wait JSON consistent with harness availability.
// Copy the wait so control-plane reset authority and observation history survive.
func (s *Scheduler) runSnapshot(run workflow.Run) workflow.Run {
	if wait := run.HarnessWait; wait != nil && wait.Reason == "credits_exhausted" && !s.deps.Now().Before(wait.ResetAt) {
		current := *wait
		current.ResetAt = time.Time{}
		current.ResetTimeSource = ""
		run.HarnessWait = &current
	}
	return run
}
