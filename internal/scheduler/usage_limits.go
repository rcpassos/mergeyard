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
	"github.com/rcpassos/mergeyard/internal/execution/durable"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

const waitsExhausted = "harness.usage_limit_waits_exhausted"

type temporaryLimitError = durable.TemporaryLimitError
type creditLimitError = durable.CreditLimitError

func (s *Scheduler) parseExecution(ctx context.Context, runID, attemptID, agent string, phase harness.PhaseContext, artifacts harness.PhaseArtifacts) (harness.PhaseResult, error) {
	return s.execution().Parse(ctx, runID, attemptID, agent, phase, artifacts)
}

func isHarnessLimit(err error) bool {
	var temporary *temporaryLimitError
	var credit *creditLimitError
	return errors.As(err, &temporary) || errors.As(err, &credit)
}
func (s *Scheduler) creditExecution(ctx context.Context, repo config.Repository, run workflow.Run, a attempt, cause *creditLimitError) error {
	wait := workflow.HarnessWait{Harness: a.agent, Reason: "credits_exhausted", Phase: run.Phase, Round: phaseRound(run, run.Phase), AttemptID: a.id, Attempt: a.number, ExitCode: &cause.Exit, DetectedAt: s.deps.Now().UTC(), Source: cause.Outcome.Source}
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
	wait := workflow.HarnessWait{Harness: a.agent, Reason: "temporary_limit", Phase: run.Phase, Round: phaseRound(run, run.Phase), AttemptID: a.id, Attempt: a.number, ExitCode: &limit.Exit, DetectedAt: now, ResetAt: now.Add(s.cfg.UsageLimits.Cooldown), ResetTimeSource: "default_cooldown", Source: limit.Outcome.Source, Allowance: s.cfg.UsageLimits.MaxWaits}
	if limit.Outcome.ResetAt.After(now) {
		wait.ResetAt = limit.Outcome.ResetAt.UTC()
		wait.ResetTimeSource = "reported"
	}
	var lastOrdinary int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(max(attempt),0) FROM phase_attempts WHERE run_id=? AND phase=? AND round=? AND step='' AND status NOT IN ('running','usage_limited')`, run.ID, run.Phase, wait.Round).Scan(&lastOrdinary); err != nil {
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
	Check                     *workflow.HarnessCheck `json:"check,omitempty"`
	CheckEligible             bool                   `json:"check_eligible"`
	Reason                    string                 `json:"reason,omitempty"`
	ProbeID                   string                 `json:"probe_id,omitempty"`
	ProbeRunID                string                 `json:"probe_run_id,omitempty"`
	ProbeAttemptID            string                 `json:"probe_attempt_id,omitempty"`
	Harness                   string                 `json:"harness"`
	Available                 bool                   `json:"available"`
	ResetAt                   time.Time              `json:"reset_at,omitzero"`
	ResetTimeSource           string                 `json:"reset_time_source,omitempty"`
	Source                    string                 `json:"source,omitempty"`
	TemporaryLimitDetection   bool                   `json:"temporary_limit_detection"`
	CreditExhaustionDetection bool                   `json:"credit_exhaustion_detection"`
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
		now := s.deps.Now()
		v.Available = limit.Available(now)
		if limit != nil {
			current := (workflow.HarnessWait{Reason: limit.Reason, ResetAt: limit.ResetAt, ResetTimeSource: limit.ResetTimeSource}).Display(now)
			v.ResetAt, v.ResetTimeSource, v.Source = current.ResetAt, current.ResetTimeSource, limit.Source
			v.Reason, v.ProbeID, v.ProbeRunID, v.ProbeAttemptID = limit.Reason, limit.ProbeID, limit.ProbeRunID, limit.ProbeAttemptID
		}
		v.CheckEligible = limit.CheckEligible(now, caps.CreditExhaustionDetection)
		checks, err := workflow.LoadHarnessChecks(ctx, s.db, name, false)
		if err != nil {
			return nil, err
		}
		if len(checks) > 0 {
			v.Check = &checks[len(checks)-1]
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
	return limit.Gate(s.deps.Now()), nil
}

// harnessLimited reports whether an account-wide usage limit withholds one
// harness. It reads only local state, so it can run before and after the claim
// reads that cost GitHub calls.
func (s *Scheduler) harnessLimited(ctx context.Context, agent string) (bool, error) {
	if s.db == nil {
		return false, nil
	}
	gate, err := s.harnessGate(ctx, agent)
	if err != nil {
		return false, err
	}
	return gate != nil, nil
}

// gateLimitedIssue withholds a candidate whose implementer harness is limited,
// resolving that harness from the labels the issue already carries. A gated
// candidate is never claimed, so this keeps the trust, blocker and fresh-read
// calls that would otherwise repeat on every tick.
func (s *Scheduler) gateLimitedIssue(ctx context.Context, repo config.Repository, issue github.Issue) (bool, error) {
	settings, err := s.resolveIssueRoleSettings(repo, issue)
	if err != nil {
		// An override or role configuration that does not resolve is refused from
		// the fresh snapshot, so its implementer is not guessed at here.
		return false, nil
	}
	return s.harnessLimited(ctx, settings.Implementer.Agent)
}

func (s *Scheduler) gatePhase(ctx context.Context, run workflow.Run, agent string) (bool, error) {
	limit, err := workflow.LoadHarnessLimit(ctx, s.db, agent)
	if err != nil {
		return false, err
	}
	now := s.deps.Now()
	wait := limit.Gate(now)
	if wait == nil || limit.ProbeReady(run.ID, now, s.harnesses[agent].Capabilities().CreditExhaustionDetection) {
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
	limit, err := workflow.LoadHarnessLimit(ctx, s.db, wait.Harness)
	if err != nil {
		return err
	}
	now := s.deps.Now()
	if !limit.Available(now) && !limit.ProbeReady(run.ID, now, s.harnesses[wait.Harness].Capabilities().CreditExhaustionDetection) {
		return nil
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
	if !limit.CreditBlocked() {
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
	repo, ok := s.runRepository(run)
	if !ok {
		return false, nil
	}
	// Retry of failed checks observes commands before needing a harness; manual
	// corrections may pass even while the implementer's account stays blocked.
	if run.LastErrorCode == "checks.failed" {
		return true, nil
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
	if limit.CreditBlocked() {
		return limit.RecoveryEligible(s.harnesses[agent].Capabilities().CreditExhaustionDetection), nil
	}
	return true, nil
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
	if err == nil && a.status == "succeeded" {
		if repo, ok := s.runRepository(run); ok && len(repo.Checks) > 0 {
			if checks := latestChecks(run); checks != nil && checks.Status == "failed" && checks.OwnerID == a.id {
				return true, nil
			}
		}
	}
	return a.status != "succeeded", err
}

// runSnapshot keeps current credit-wait JSON consistent with harness availability.
// Copy the wait so control-plane reset authority and observation history survive.
func (s *Scheduler) runSnapshot(run workflow.Run) workflow.Run {
	if run.HarnessWait != nil {
		current := run.HarnessWait.Display(s.deps.Now())
		run.HarnessWait = &current
	}
	return run
}
