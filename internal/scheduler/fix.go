package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/execution/durable"
	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/review"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// FixGit separates the replayable commit from a push of its durable pinned SHA.
type FixGit interface {
	CommitFix(context.Context, managedgit.Run, managedgit.Phase, string) (managedgit.CommitResult, error)
	PushFix(context.Context, managedgit.Run, string, string) error
	InspectFixTarget(context.Context, managedgit.Run, string) error
}

type fixAttempt struct {
	attempt
	target, commit string
	pushed         bool
	findings       []review.Finding
	report         *review.FixReport
	ci             *ci.Snapshot
}

func (s *Scheduler) lastFix(ctx context.Context, run workflow.Run) (fixAttempt, error) {
	var a fixAttempt
	saved, err := s.execution().Load(ctx, run.ID, workflow.Fix, run.ReviewRound)
	if err != nil {
		return a, err
	}
	a.attempt = schedulerAttempt(saved)
	var findings string
	var report, diagnostics sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT target_sha,COALESCE(commit_sha,''),pushed,findings_json,report_json,ci_json FROM fix_attempts WHERE attempt_id=?`, a.id).Scan(&a.target, &a.commit, &a.pushed, &findings, &report, &diagnostics)
	if err != nil {
		return a, err
	}
	if err := json.Unmarshal([]byte(findings), &a.findings); err != nil {
		return a, err
	}
	if report.Valid {
		a.report = &review.FixReport{}
		if err := json.Unmarshal([]byte(report.String), a.report); err != nil {
			return a, err
		}
	}
	if diagnostics.Valid {
		a.ci = &ci.Snapshot{}
		err = json.Unmarshal([]byte(diagnostics.String), a.ci)
	}
	return a, err
}

func (s *Scheduler) fix(ctx context.Context, repo config.Repository, run workflow.Run) error {
	fail := func(err error) error { return s.recordAttention(ctx, repo, run, err) }
	if run.ReviewRound >= s.roundLimit(run) {
		return fail(&fault.Error{Code: "review.max_rounds_exceeded", Message: "No subsequent review round is available; fix was not launched"})
	}
	_, gitRun, err := s.context(ctx, run)
	if err != nil {
		return fail(err)
	}
	a, err := s.lastFix(ctx, run)
	if errors.Is(err, sql.ErrNoRows) {
		return s.startFix(ctx, repo, run, gitRun, 1)
	}
	if err != nil {
		return fail(err)
	}
	if next := explicitAttempt(run, workflow.Fix, a.number); next > 0 {
		return s.startFix(ctx, repo, run, gitRun, next)
	}
	if a.status == "usage_limited" {
		return s.startFix(ctx, repo, run, gitRun, a.number+1)
	}
	if a.status == "failed" {
		code, message, _ := strings.Cut(a.failure, ": ")
		cause := &fault.Error{Code: code, Message: message}
		if code == "phase.blocked" && a.report != nil {
			cause.HumanAction = a.report.HumanAction
		}
		retry, retryErr := s.retryPhase(ctx, run, workflow.Fix, a.attempt, cause, repo.Implementer.MaxAttempts)
		if retryErr != nil {
			return retryErr
		}
		if !retry {
			return fail(fixFailure(cause, a.number, repo.Implementer.MaxAttempts))
		}
		return s.startFix(ctx, repo, run, gitRun, a.number+1)
	}
	if a.status == "running" {
		pr, err := s.deps.GitHub.GetPullRequest(ctx, repo.Repo, run.PRNumber)
		if err != nil {
			return err
		}
		if pr != nil && pr.Merged {
			return s.merged(ctx, repo, run, pr)
		}
		if !reviewHeadMatches(pr, repo, run, gitRun, a.target) {
			if err := s.deps.Runner.StopSession(ctx, a.ref); err != nil {
				return err
			}
			return s.finishFixAttempt(ctx, repo, run, a, nil, nil, &fault.Error{Code: "review.head_changed", Message: "PR head changed during fix; preserved work needs inspection"})
		}
		executionAttempt := durableAttempt(a.attempt)
		observation, err := s.execution().Observe(ctx, run.ID, workflow.Fix, &executionAttempt)
		a.attempt = schedulerAttempt(executionAttempt)
		if err != nil {
			if observation.Failure != nil || ctx.Err() != nil {
				return err
			}
			return fail(err)
		}
		status := observation.Status
		if status.State == runner.SessionRunning && observation.Failure == nil {
			return nil
		}
		cause := observation.Failure
		var report *review.FixReport
		if cause == nil {
			result, err := s.parseExecution(ctx, run.ID, a.id, a.agent, harness.PhaseContext{Phase: workflow.Fix, PhaseDir: a.ref.PhaseDir, SessionID: a.sessionID, Resume: a.resumed}, observation.Artifacts)
			cause = err
			if err == nil {
				report = &review.FixReport{HumanAction: result.HumanAction, SchemaVersion: result.SchemaVersion, Status: result.Status, Summary: result.Summary, Responses: result.Responses, Verification: result.Verification}
				_ = json.Unmarshal(result.Criteria, &report.Criteria)
				if err := report.ValidateFindings(a.findings); err != nil {
					cause = &fault.Error{Code: "phase.result_invalid", Message: "Fix responses do not match supplied findings", Err: err}
					report = nil
				} else {
					data, _ := json.Marshal(report)
					if err := s.deps.Runner.WriteFile(ctx, filepath.Join(a.ref.PhaseDir, "result.json"), data, 0600); err != nil {
						return err
					}
					if report.Status != "success" {
						cause = &fault.Error{Code: "phase." + report.Status, Message: report.Summary, HumanAction: report.HumanAction}
					}
				}
			}
		}
		// Observe again after parsing before any commit or publication.
		pr, err = s.deps.GitHub.GetPullRequest(ctx, repo.Repo, run.PRNumber)
		if err != nil {
			return err
		}
		if pr != nil && pr.Merged {
			return s.merged(ctx, repo, run, pr)
		}
		if !reviewHeadMatches(pr, repo, run, gitRun, a.target) {
			cause = &fault.Error{Code: "review.head_changed", Message: "PR head changed during fix; preserved work needs inspection"}
		}
		return s.finishFixAttempt(ctx, repo, run, a, status.ExitCode, report, cause)
	}
	if a.status != "succeeded" {
		return nil
	}
	if a.report == nil || a.report.Status != "success" {
		return fail(&fault.Error{Code: "phase.result_invalid", Message: "Completed fix is missing its success report"})
	}
	g, ok := s.deps.Git.(FixGit)
	if !ok {
		return fail(&fault.Error{Code: "git.fix_unsupported", Message: "Git adapter cannot journal fix publication"})
	}
	if a.commit == "" {
		// Check current PR identity before writing, even when a previous tick validated it.
		pr, err := s.deps.GitHub.GetPullRequest(ctx, repo.Repo, run.PRNumber)
		if err != nil {
			return err
		}
		if pr != nil && pr.Merged {
			return s.merged(ctx, repo, run, pr)
		}
		if !reviewHeadMatches(pr, repo, run, gitRun, a.target) {
			return fail(&fault.Error{Code: "review.head_changed", Message: "PR head changed before fix publication"})
		}
		result, err := g.CommitFix(ctx, gitRun, managedgit.Phase{Title: fmt.Sprintf("fix review round %d", run.ReviewRound)}, a.target)
		if diagnosticErr := s.recordStagingDiagnostics(ctx, run, result); diagnosticErr != nil {
			return fail(diagnosticErr)
		}
		if err != nil {
			return fail(err)
		}
		if !result.TreeChanged {
			if a.ci != nil && a.ci.RepairCause == "git.base_conflict" {
				baseGit, ok := s.deps.Git.(BaseGit)
				if !ok {
					return fail(&fault.Error{Code: "git.base_check_unsupported", Message: "Git adapter cannot establish conflict resolution"})
				}
				merged, err := baseGit.CheckBase(ctx, gitRun, a.ci.Base.BaseBranch, result.SHA)
				if err != nil {
					return fail(err)
				}
				if len(merged.Conflicts) > 0 {
					return s.finishFixAttempt(ctx, repo, run, a, nil, a.report, &fault.Error{Code: "phase.result_invalid", Message: "Fix reports success but still conflicts with the current base"})
				}
			} else if a.ci != nil {
				return s.finishFixAttempt(ctx, repo, run, a, nil, a.report, &fault.Error{Code: "phase.result_invalid", Message: "CI repair reports success but made no code changes"})
			}
			for _, response := range a.report.Responses {
				if response.Resolution == "fixed" {
					return s.finishFixAttempt(ctx, repo, run, a, nil, a.report, &fault.Error{Code: "phase.result_invalid", Message: "Fix claims a fixed finding but made no code changes"})
				}
			}
		}
		_, err = s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
			_, err := tx.ExecContext(ctx, "UPDATE fix_attempts SET commit_sha=? WHERE attempt_id=?", result.SHA, a.id)
			return events.Draft{RunID: run.ID, Type: "fix.committed", Payload: map[string]any{"round": run.ReviewRound, "attempt": a.number, "commit_sha": result.SHA, "committed": result.Committed}}, err
		})
		return err // Separate reconciliation steps make each crash boundary durable.
	}
	if !a.pushed && (len(repo.Checks) > 0 || runningSystemStep(run, "checks")) {
		v, done, err := s.systemStep(ctx, repo, run, gitRun, systemStepRequest{Step: "checks", Commands: repo.Checks, TargetSHA: a.commit, OwnerID: a.id, Retry: s.recheckRequested(run, a.id)})
		if err != nil {
			return fail(err)
		}
		if !done {
			return nil
		}
		if v.Status != "succeeded" {
			if err := s.repairChecks(ctx, repo, run, gitRun, a.attempt, v); err != nil {
				return fail(err)
			}
			return nil
		}
	}
	if !a.pushed {
		pr, err := s.deps.GitHub.GetPullRequest(ctx, repo.Repo, run.PRNumber)
		if err != nil {
			return err
		}
		if pr != nil && pr.Merged {
			return s.merged(ctx, repo, run, pr)
		}
		if !reviewHeadMatches(pr, repo, run, gitRun, a.target) && !reviewHeadMatches(pr, repo, run, gitRun, a.commit) {
			return fail(&fault.Error{Code: "review.head_changed", Message: "PR head changed during fix publication"})
		}
		if err := g.PushFix(ctx, gitRun, a.target, a.commit); err != nil {
			return fail(err)
		}
		_, err = s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
			_, err := tx.ExecContext(ctx, "UPDATE fix_attempts SET pushed=1 WHERE attempt_id=?", a.id)
			return events.Draft{RunID: run.ID, Type: "fix.pushed", Payload: map[string]any{"round": run.ReviewRound, "attempt": a.number, "commit_sha": a.commit}}, err
		})
		return err
	}
	pr, err := s.deps.GitHub.GetPullRequest(ctx, repo.Repo, run.PRNumber)
	if err != nil {
		return err
	}
	if pr != nil && pr.Merged {
		return s.merged(ctx, repo, run, pr)
	}
	if !reviewHeadMatches(pr, repo, run, gitRun, a.commit) {
		return fail(&fault.Error{Code: "review.head_changed", Message: "PR head changed before the next review"})
	}
	if err := g.InspectFixTarget(ctx, gitRun, a.commit); err != nil {
		return fail(err)
	}
	approved := ""
	_, err = s.workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.FixSucceeded, Metadata: workflow.MetadataPatch{IncrementReviewRound: true, ApprovedSHA: &approved, Fix: &workflow.FixCompletion{AttemptID: a.id}}})
	return err
}

func (s *Scheduler) finishFixAttempt(ctx context.Context, repo config.Repository, run workflow.Run, a fixAttempt, exit *int, report *review.FixReport, cause error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if isHarnessLimit(cause) {
		return s.limitExecution(ctx, repo, run, a.attempt, cause)
	}
	var data any
	if report != nil {
		encoded, err := json.Marshal(report)
		if err != nil {
			return err
		}
		data = string(encoded)
	}
	status, event, failure := "succeeded", "phase.completed", ""
	if cause != nil {
		status, event, failure = "failed", "phase.failed", cause.Error()
	}
	_, err := s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		if _, err := tx.ExecContext(ctx, "UPDATE fix_attempts SET report_json=? WHERE attempt_id=?", data, a.id); err != nil {
			return events.Draft{}, err
		}
		_, err := tx.ExecContext(ctx, `UPDATE phase_attempts SET status=?,exit_code=?,error=?,ended_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, status, exit, failure, a.id)
		return events.Draft{RunID: run.ID, Type: event, Payload: map[string]any{"agent": a.agent, "phase": workflow.Fix, "round": run.ReviewRound, "attempt": a.number, "result": report, "error": failure}}, err
	})
	if err != nil {
		return err
	}
	if cause != nil {
		retry, retryErr := s.retryPhase(ctx, run, workflow.Fix, a.attempt, cause, repo.Implementer.MaxAttempts)
		if retryErr != nil {
			return retryErr
		}
		if retry {
			return nil
		}
		return s.recordAttention(ctx, repo, run, fixFailure(cause, a.number, repo.Implementer.MaxAttempts))
	}
	return nil
}

func fixFailure(cause error, number, max int) error {
	code := errorCodeForReview(cause)
	if code == "phase.execution_failed" && number >= max {
		code = "phase.retries_exhausted"
	}
	return &fault.Error{Code: code, Message: fmt.Sprintf("Fix attempt %d could not be accepted: %s", number, cause.Error()), Err: cause}
}

func (s *Scheduler) startFix(ctx context.Context, repo config.Repository, run workflow.Run, gitRun managedgit.Run, number int, checkRetry ...string) error {
	if gated, err := s.gatePhase(ctx, run, repo.Implementer.Agent); gated || err != nil {
		return err
	}
	fail := func(err error) error { return s.recordAttention(ctx, repo, run, err) }
	if run.Implementer != nil {
		if err := validateImplementerHarness(run.Implementer.Agent, repo.Implementer.Agent); err != nil {
			return fail(err)
		}
	}
	if run.Review == nil || !run.Review.Accepted || run.Review.Report == nil || run.Review.Round != run.ReviewRound {
		return fail(&fault.Error{Code: "phase.result_invalid", Message: "Fix requires an accepted current review"})
	}
	var repair *ci.Snapshot
	if run.Review.Report.Status == "approved" && run.CI != nil && run.CI.SHA == run.Review.TargetSHA && run.CI.QueryError == "" {
		_, cause := run.CI.Evidence.Gate()
		if ((cause == "ci.check_failed" || cause == "ci.check_timed_out") && run.CI.RepairCause == cause) || (baseRepairCause(*run.CI) != "" && baseRepairCause(*run.CI) == run.CI.RepairCause) {
			repair = run.CI
		}
	}
	if run.Review.Report.Status != "changes_required" && repair == nil {
		return fail(&fault.Error{Code: "phase.result_invalid", Message: "Fix requires blocking findings or pinned repairable CI evidence"})
	}
	issue, _, err := s.context(ctx, run)
	if err != nil {
		return err
	}
	pr, err := s.deps.GitHub.GetPullRequest(ctx, repo.Repo, run.PRNumber)
	if err != nil {
		return err
	}
	if pr != nil && pr.Merged {
		return s.merged(ctx, repo, run, pr)
	}
	target := run.Review.TargetSHA
	if !reviewHeadMatches(pr, repo, run, gitRun, target) {
		return fail(&fault.Error{Code: "review.head_changed", Message: "PR head changed before fix launch"})
	}
	if repair != nil && repair.RepairCause == "git.base_conflict" {
		if !repair.Base.MergePrepared {
			g, ok := s.deps.Git.(interface {
				PrepareBaseRepair(context.Context, managedgit.Run, string, string) error
			})
			if !ok {
				return fail(&fault.Error{Code: "git.base_check_unsupported", Message: "Git adapter cannot prepare conflict repair"})
			}
			wait := *repair
			base := *repair.Base
			wait.Base = &base
			base.MergeStarted = true
			if err := s.saveCI(ctx, run, wait, "ci.updated"); err != nil {
				return err
			}
			if err := g.PrepareBaseRepair(ctx, gitRun, target, base.BaseSHA); err != nil {
				return fail(err)
			}
			base.MergePrepared = true
			if err := s.saveCI(ctx, run, wait, "ci.updated"); err != nil {
				return err
			}
			repair = &wait
		}
	}
	findings := []review.Finding{}
	for _, f := range run.Review.Report.Findings {
		if f.Severity == "blocking" {
			findings = append(findings, f)
		}
	}
	phase, previous, err := s.execution().Prepare(ctx, s.workspace.Root, run, workflow.Fix, repo.Implementer, number)
	if err != nil {
		return fail(err)
	}
	phaseDir, sessionID := phase.PhaseDir, phase.SessionID
	warning := ""
	if strings.HasPrefix(previous.Failure, "harness.session_resume_failed: ") {
		warning = "harness.session_resume_failed"
	}
	previousSession := previous.SessionID
	if phase.Resume {
		previousSession = phase.SessionID
	}
	env, err := s.phaseEnvironment(ctx, run.ID, workflow.Fix)
	if err != nil {
		return fail(err)
	}
	phase.WorktreePath, phase.Env = gitRun.Path, env
	phase.Interruption = interruptionContext(schedulerAttempt(previous), phase)
	checkID := checkRetryIdentity(run, workflow.Fix, schedulerAttempt(previous), number, checkRetry)
	issueInput, err := s.phaseIssueInput(ctx, repo, run.ID, issue, gitRun.BaseBranch, &phase)
	if err != nil {
		return fail(err)
	}
	input, err := harness.WriteFixInput(ctx, s.deps.Runner, phase, harness.FixInput{Issue: issueInput, PRNumber: pr.Number, PRURL: pr.URL, PRBody: pr.Body, TargetSHA: target, Round: run.ReviewRound, Findings: findings, CI: repair, Checks: s.checkResult(run, checkID)})
	if err != nil {
		return fail(err)
	}
	command, err := s.harnesses[repo.Implementer.Agent].BuildInvocation(phase, repo.Implementer)
	if err != nil {
		return fail(err)
	}
	req := runner.SessionRequest{RunID: run.ID, Phase: "fix", Round: run.ReviewRound, Attempt: number, PhaseDir: phaseDir, Command: command}
	permission, allowedTools := s.roleSettings(repo.Implementer.Agent)
	tools, _ := json.Marshal(allowedTools)
	encoded, _ := json.Marshal(findings)
	var diagnostics any
	if repair != nil {
		data, err := json.Marshal(repair)
		if err != nil {
			return err
		}
		diagnostics = string(data)
	}
	launched, err := s.execution().Launch(ctx, run, repo.Implementer, s.rolePermissions(repo.Implementer.Agent), phase, previous, req, input, checkID, func(tx *sql.Tx, a durable.Attempt) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO fix_attempts(attempt_id,session_id,target_sha,findings_json,permission_mode,allowed_tools_json,ci_json) VALUES (?,?,?,?,?,?,?)`, a.ID, sessionID, target, string(encoded), permission, string(tools), diagnostics)
		return err
	}, map[string]any{"agent": repo.Implementer.Agent, "permissions": s.rolePermissions(repo.Implementer.Agent), "session_id": sessionID, "model": repo.Implementer.Model, "effort": repo.Implementer.Effort, "skills": repo.Implementer.Skills, "permission_mode": permission, "allowed_tools": allowedTools, "warning_code": warning, "previous_session_id": previousSession, "ci": repair})
	if err != nil && launched.ID != "" && launched.Status == "failed" && ctx.Err() == nil {
		return s.finishFixAttempt(ctx, repo, run, fixAttempt{attempt: schedulerAttempt(launched)}, nil, nil, err)
	}
	return err
}
