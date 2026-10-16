package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/execution/durable"
	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/review"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/verification"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// ReviewGit extends managed ownership with a durable, reversible review boundary.
type ReviewGit interface {
	SnapshotReview(context.Context, managedgit.Run) (managedgit.ReviewSnapshot, error)
	ReviewDiff(context.Context, managedgit.Run, string) (string, error)
	ReviewChanged(context.Context, managedgit.Run, managedgit.ReviewSnapshot) (bool, error)
	RestoreReview(context.Context, managedgit.Run, managedgit.ReviewSnapshot) error
}
type reviewAttempt struct {
	attempt
	target                            string
	snapshot                          managedgit.ReviewSnapshot
	restoring, restored, contaminated bool
}

func (s *Scheduler) lastReview(ctx context.Context, run workflow.Run) (reviewAttempt, error) {
	var a reviewAttempt
	saved, err := s.execution().Load(ctx, run.ID, workflow.Review, run.ReviewRound)
	if err != nil {
		return a, err
	}
	a.attempt = schedulerAttempt(saved)
	var snapshot string
	err = s.db.QueryRowContext(ctx, `SELECT target_sha,snapshot_json,restoration_started,restored,contaminated FROM review_attempts WHERE attempt_id=?`, a.id).Scan(&a.target, &snapshot, &a.restoring, &a.restored, &a.contaminated)
	if err != nil {
		return a, err
	}
	err = json.Unmarshal([]byte(snapshot), &a.snapshot)
	return a, err
}
func (s *Scheduler) review(ctx context.Context, repo config.Repository, run workflow.Run) error {
	issue, gitRun, err := s.context(ctx, run)
	if err != nil {
		return s.recordAttention(ctx, repo, run, err)
	}
	a, err := s.lastReview(ctx, run)
	if errors.Is(err, sql.ErrNoRows) {
		return s.startReview(ctx, repo, run, issue, gitRun, 1)
	}
	if err != nil {
		return s.recordAttention(ctx, repo, run, err)
	}
	if (a.restoring || a.status == "failed") && !a.restored {
		if err := s.restoreReview(ctx, gitRun, &a); err != nil {
			return s.recordAttention(ctx, repo, run, err)
		}
	}
	if next := explicitAttempt(run, workflow.Review, a.number); next > 0 {
		return s.startReview(ctx, repo, run, issue, gitRun, next)
	}
	if a.status == "usage_limited" {
		return s.startReview(ctx, repo, run, issue, gitRun, a.number+1)
	}
	if a.status == "failed" {
		code, message, _ := strings.Cut(a.failure, ": ")
		cause := &fault.Error{Code: code, Message: message}
		retry, retryErr := s.retryPhase(ctx, run, workflow.Review, a.attempt, cause, repo.Reviewer.MaxAttempts)
		if retryErr != nil {
			return retryErr
		}
		if !retry {
			return s.recordAttention(ctx, repo, run, reviewFailure(cause, a.number))
		}
		return s.startReview(ctx, repo, run, issue, gitRun, a.number+1)
	}
	if a.status != "running" {
		return nil
	}
	pr, err := s.deps.GitHub.GetPullRequest(ctx, repo.Repo, run.PRNumber)
	if err != nil {
		return err
	}
	if pr != nil && pr.Merged {
		return s.merged(ctx, repo, run, pr)
	}
	if !reviewHeadMatches(pr, repo, run, gitRun, a.target) {
		return s.abortReview(ctx, repo, run, gitRun, &a, &fault.Error{Code: "review.head_changed", Message: "PR head changed during review; stale verdict discarded. Inspect preserved work before continuing"})
	}
	executionAttempt := durableAttempt(a.attempt)
	observation, err := s.execution().ObserveProcess(ctx, run.ID, workflow.Review, &executionAttempt)
	a.attempt = schedulerAttempt(executionAttempt)
	if err != nil {
		return err
	}
	status := observation.Status
	if observation.Failure != nil && invalidSessionDiscovery(observation.Failure) {
		return s.abortReview(ctx, repo, run, gitRun, &a, observation.Failure)
	}
	if status.State == runner.SessionRunning {
		return nil
	}

	// Journal contamination before any restoring mutation. On restart the flag
	// survives even if Git was already fully restored before SQLite was updated.
	if err := s.restoreReview(ctx, gitRun, &a); err != nil {
		return s.recordAttention(ctx, repo, run, err)
	}
	var report *review.Report
	cause := observation.Failure
	if cause == nil {
		observation = s.execution().ReadArtifacts(ctx, executionAttempt, observation)
		cause = observation.Failure
		if cause == nil {
			result, err := s.parseExecution(ctx, run.ID, a.id, a.agent, harness.PhaseContext{Phase: workflow.Review, PhaseDir: a.ref.PhaseDir, SessionID: a.sessionID, Resume: a.resumed}, observation.Artifacts)
			cause = err
			if err == nil {
				report = &review.Report{HumanAction: result.HumanAction, SchemaVersion: result.SchemaVersion, Status: result.Status, Summary: result.Summary, Findings: result.Findings, PriorFindings: result.PriorFindings}
				_ = json.Unmarshal(result.Criteria, &report.Criteria)
				previous, fix, err := review.LoadPriorContext(ctx, s.db, run.ID, run.ReviewRound)
				if err != nil {
					return err
				}
				if err := report.ValidatePriorFindings(run.ReviewRound, previous, fix); err != nil {
					cause = &fault.Error{Code: "phase.result_invalid", Message: "Invalid prior finding outcomes", Err: err}
				}
				data, _ := json.Marshal(report)
				if err := s.deps.Runner.WriteFile(ctx, filepath.Join(a.ref.PhaseDir, "result.json"), data, 0600); err != nil {
					return err
				}
				if cause == nil && (report.Status == "blocked" || report.Status == "failed") {
					cause = &fault.Error{Code: "phase." + report.Status, Message: report.Summary, HumanAction: report.HumanAction}
					if report.Summary == "" {
						cause = &fault.Error{Code: "phase." + report.Status, Message: "Reviewer reported " + report.Status, HumanAction: report.HumanAction}
					}
				}
			}
		}
	}
	if a.contaminated && !isHarnessLimit(cause) {
		cause = &fault.Error{Code: "review.code_changed", Message: "Reviewer changed repository files, index, or commits; restored pre-review work and discarded verdict"}
	}
	// Fetch again after restoration and parsing. Never authorize a different head.
	pr, err = s.deps.GitHub.GetPullRequest(ctx, repo.Repo, run.PRNumber)
	if err != nil {
		return err
	}
	if pr != nil && pr.Merged {
		return s.merged(ctx, repo, run, pr)
	}
	if !reviewHeadMatches(pr, repo, run, gitRun, a.target) {
		cause = &fault.Error{Code: "review.head_changed", Message: "PR head changed during review; stale verdict discarded"}
	}
	if isHarnessLimit(cause) {
		return s.limitExecution(ctx, repo, run, a.attempt, cause)
	}
	if cause != nil {
		retry, retryErr := s.retryPhase(ctx, run, workflow.Review, a.attempt, cause, repo.Reviewer.MaxAttempts)
		if retryErr != nil {
			return retryErr
		}
		if !retry {
			return s.rejectReview(ctx, repo, run, a, status.ExitCode, report, reviewFailure(cause, a.number))
		}
		return s.failReview(ctx, run, a, status.ExitCode, report, cause)
	}
	trigger := workflow.ReviewApproved
	approved := a.target
	var failure *fault.Error
	if report.Status == "changes_required" {
		approved = ""
		trigger = workflow.ReviewChangesRequired
		if run.ReviewRound >= s.roundLimit(run) {
			trigger = workflow.ReviewRoundsExhausted
			failure = &fault.Error{Code: "review.max_rounds_exceeded", Message: "Review requested changes with no review rounds remaining"}
		}
	}
	var wait *ci.Snapshot
	if trigger == workflow.ReviewApproved {
		now := s.deps.Now().UTC()
		wait = &ci.Snapshot{SHA: approved, StartedAt: now, Deadline: now.Add(s.cfg.CITimeout)}
	}
	_, err = s.workflow.Transition(ctx, run.ID, workflow.Request{Trigger: trigger, Failure: failure, Metadata: workflow.MetadataPatch{CI: wait, ApprovedSHA: &approved, Review: &workflow.ReviewCompletion{AttemptID: a.id, Report: *report, ExitCode: *status.ExitCode}}})
	if err != nil {
		return err
	}
	if failure != nil {
		return s.attentionLabels(ctx, repo, run)
	}
	return nil
}
func reviewHeadMatches(pr *github.PullRequest, repo config.Repository, run workflow.Run, gitRun managedgit.Run, target string) bool {
	return pr != nil && pr.Number == run.PRNumber && pr.State == github.Open && pr.Head.SHA == target && pr.Head.Ref == gitRun.Branch && strings.EqualFold(pr.Head.Repo.FullName, repo.Repo)
}
func (s *Scheduler) startReview(ctx context.Context, repo config.Repository, run workflow.Run, issue github.Issue, gitRun managedgit.Run, number int) error {
	fail := func(err error) error { return s.recordAttention(ctx, repo, run, err) }
	if gated, err := s.gatePhase(ctx, run, repo.Reviewer.Agent); gated || err != nil {
		return err
	}

	g, ok := s.deps.Git.(ReviewGit)
	if !ok {
		return fail(&fault.Error{Code: "git.review_unsupported", Message: "Git adapter cannot protect reviewer changes"})
	}
	var previous reviewAttempt
	var err error
	if number > 1 {
		previous, err = s.lastReview(ctx, run)
		if err != nil {
			return err
		}
		if err := s.restoreReview(ctx, gitRun, &previous); err != nil {
			return fail(err)
		}
	}
	target := ""
	var selectedAt time.Time
	selectTarget := func(sha, requested string) error {
		requestedAt, err := time.Parse(time.RFC3339Nano, requested)
		if err != nil {
			return &fault.Error{Code: "review.selection_invalid", Message: "Cannot order saved Retry and handback selections; inspect recovery history"}
		}
		if !requestedAt.Before(selectedAt) {
			target, selectedAt = sha, requestedAt
		}
		return nil
	}
	for _, v := range run.Retries {
		if !v.Pending && v.Error == "" && v.NextPhase == workflow.Review && v.Round == run.ReviewRound && v.TargetSHA != "" {
			if err := selectTarget(v.TargetSHA, v.RequestedAt); err != nil {
				return fail(err)
			}
		}
	}
	for _, v := range run.Handbacks {
		if !v.Pending && v.Error == "" && v.NextPhase == workflow.Review && v.Round == run.ReviewRound {
			if err := selectTarget(v.CommitSHA, v.RequestedAt); err != nil {
				return fail(err)
			}
		}
	}
	hasReconciledTarget := target != ""
	if run.ReviewRound > 1 || target != "" {
		fixer, ok := s.deps.Git.(FixGit)
		if !ok || (run.Fix == nil && target == "") {
			return fail(&fault.Error{Code: "git.fix_unsupported", Message: "Cannot verify the pinned fix before re-review"})
		}
		if target == "" {
			target = run.Fix.CommitSHA
		}
		if err := fixer.InspectFixTarget(ctx, gitRun, target); err != nil {
			return fail(err)
		}
	}
	snapshot, err := g.SnapshotReview(ctx, gitRun)
	if err != nil {
		return fail(err)
	}
	if previous.status == "usage_limited" && snapshot.Head != previous.target && (!hasReconciledTarget || snapshot.Head != target) {
		return fail(&fault.Error{Code: "review.head_changed", Message: "Review target changed during usage-limit waiting; inspect preserved work"})
	}
	if hasReconciledTarget && snapshot.Head != target {
		return fail(&fault.Error{Code: "review.head_changed", Message: "Review target differs from the reconciled target"})
	}
	pr, err := s.deps.GitHub.GetPullRequest(ctx, repo.Repo, run.PRNumber)
	if err != nil {
		return err
	}
	if pr != nil && pr.Merged {
		return s.merged(ctx, repo, run, pr)
	}
	if !reviewHeadMatches(pr, repo, run, gitRun, snapshot.Head) {
		return fail(&fault.Error{Code: "review.head_changed", Message: "Local review target differs from the PR head; inspect preserved work"})
	}
	diff, err := g.ReviewDiff(ctx, gitRun, snapshot.Head)
	if err != nil {
		return fail(err)
	}
	phase, savedPrevious, err := s.execution().Prepare(ctx, s.workspace.Root, run, workflow.Review, repo.Reviewer, number)
	if err != nil {
		return fail(err)
	}
	phaseDir := phase.PhaseDir
	sessionID := phase.SessionID
	warning := ""
	if strings.HasPrefix(savedPrevious.Failure, "harness.session_resume_failed: ") {
		warning = "harness.session_resume_failed"
	}
	previousSession := savedPrevious.SessionID
	if phase.Resume {
		previousSession = phase.SessionID
	}
	env, err := s.phaseEnvironment(ctx, run.ID, workflow.Review)
	if err != nil {
		return fail(err)
	}
	phase.WorktreePath, phase.Env = gitRun.Path, env
	if previous.target == snapshot.Head {
		phase.Interruption = interruptionContext(previous.attempt, phase)
	}
	previousFindings, fixReport, err := review.LoadPriorContext(ctx, s.db, run.ID, run.ReviewRound)
	if err != nil {
		return err
	}
	var repair *ci.Snapshot
	if run.ReviewRound > 1 {
		if !hasReconciledTarget && (run.Fix == nil || !run.Fix.Pushed || run.Fix.CommitSHA != snapshot.Head || run.Fix.Round != run.ReviewRound-1) {
			return fail(&fault.Error{Code: "review.head_changed", Message: "Next review target differs from the pinned fix commit"})
		}
		if run.Fix != nil {
			repair = run.Fix.CI
		}
	}
	var checks *workflow.SystemStepSnapshot
	// Completed execution evidence remains relevant after its configured list
	// is removed. The fresh run snapshot already contains the saved results.
	for i := range run.SystemSteps {
		if run.SystemSteps[i].Step == "checks" && run.SystemSteps[i].TargetSHA == snapshot.Head {
			checks = &run.SystemSteps[i]
		}
	}
	var implementReport *harness.PhaseResult
	var resultPath string
	if err := s.db.QueryRowContext(ctx, "SELECT result_path FROM phase_attempts WHERE run_id=? AND phase='implement' AND step='' AND status='succeeded' ORDER BY attempt DESC LIMIT 1", run.ID).Scan(&resultPath); err == nil {
		raw, err := s.deps.Runner.ReadFile(ctx, resultPath)
		if err != nil {
			return fail(err)
		}
		implementReport = &harness.PhaseResult{}
		if err := json.Unmarshal(raw, implementReport); err != nil {
			return fail(err)
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fail(err)
	}
	// Already-adjudicated fixes are excluded from dispute context, but their
	// command verification remains the latest evidence on a same-round Retry.
	latestVerification := []verification.Entry{}
	// Manual PR handback can start review after a valid blocked/failed report.
	// Malformed results may retain partial reports, so include only valid completions.
	for _, attempt := range run.ImplementHistory {
		if attempt.Report != nil && (attempt.Error == "" || strings.HasPrefix(attempt.Error, "phase.blocked:") || strings.HasPrefix(attempt.Error, "phase.failed:")) {
			latestVerification = attempt.Report.Verification
		}
	}
	for _, fix := range run.FixHistory {
		if fix.Report != nil {
			latestVerification = fix.Report.Verification
		}
	}
	issueInput, err := s.phaseIssueInput(ctx, repo, run.ID, issue, gitRun.BaseBranch, &phase)
	if err != nil {
		return fail(err)
	}
	input, err := harness.WriteReviewInput(ctx, s.deps.Runner, phase, harness.ReviewInput{Verification: latestVerification, ImplementReport: implementReport, Issue: issueInput, PRNumber: pr.Number, PRURL: pr.URL, TargetSHA: snapshot.Head, BaseSHA: gitRun.BaseSHA, Diff: diff, Round: run.ReviewRound, PRBody: pr.Body, PreviousFindings: previousFindings, FixReport: fixReport, CI: repair, Checks: checks})
	if err != nil {
		return fail(err)
	}
	command, err := s.harnesses[repo.Reviewer.Agent].BuildInvocation(phase, repo.Reviewer)
	if err != nil {
		return fail(err)
	}
	req := runner.SessionRequest{RunID: run.ID, Phase: "review", Round: run.ReviewRound, Attempt: number, PhaseDir: phaseDir, Command: command}
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	permission, allowedTools := s.roleSettings(repo.Reviewer.Agent)
	tools, _ := json.Marshal(allowedTools)
	launched, err := s.execution().Launch(ctx, run, repo.Reviewer, s.rolePermissions(repo.Reviewer.Agent), phase, savedPrevious, req, input, "", func(tx *sql.Tx, a durable.Attempt) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO review_attempts(attempt_id,target_sha,diff,snapshot_json,session_id,permission_mode,allowed_tools_json) VALUES (?,?,?,?,?,?,?)`, a.ID, snapshot.Head, diff, string(snapshotJSON), sessionID, permission, string(tools))
		return err
	}, map[string]any{"agent": repo.Reviewer.Agent, "permissions": s.rolePermissions(repo.Reviewer.Agent), "target_sha": snapshot.Head, "session_id": sessionID, "model": repo.Reviewer.Model, "effort": repo.Reviewer.Effort, "skills": repo.Reviewer.Skills, "permission_mode": permission, "allowed_tools": allowedTools, "warning_code": warning, "previous_session_id": previousSession})
	if err != nil && launched.ID != "" && launched.Status == "failed" && ctx.Err() == nil {
		a := reviewAttempt{attempt: schedulerAttempt(launched), target: snapshot.Head, snapshot: snapshot}
		if restoreErr := s.restoreReview(ctx, gitRun, &a); restoreErr != nil {
			return fail(restoreErr)
		}
		retry, retryErr := s.retryPhase(ctx, run, workflow.Review, a.attempt, err, repo.Reviewer.MaxAttempts)
		if retryErr != nil {
			return retryErr
		}
		if !retry {
			return s.rejectReview(ctx, repo, run, a, nil, nil, reviewFailure(err, number))
		}
		return s.failReview(ctx, run, a, nil, nil, err)
	}
	return err
}
func (s *Scheduler) restoreReview(ctx context.Context, gitRun managedgit.Run, a *reviewAttempt) error {
	if a.restored {
		return nil
	}
	g, ok := s.deps.Git.(ReviewGit)
	if !ok {
		return &fault.Error{Code: "git.review_unsupported", Message: "Git adapter cannot restore review"}
	}
	if !a.restoring {
		changed, err := g.ReviewChanged(ctx, gitRun, a.snapshot)
		if err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, "UPDATE review_attempts SET restoration_started=1,contaminated=? WHERE attempt_id=?", changed, a.id); err != nil {
			return err
		}
		a.restoring = true
		a.contaminated = changed
	}
	if !a.contaminated {
		changed, err := g.ReviewChanged(ctx, gitRun, a.snapshot)
		if err != nil {
			return err
		}
		if changed {
			if _, err := s.db.ExecContext(ctx, "UPDATE review_attempts SET contaminated=1 WHERE attempt_id=?", a.id); err != nil {
				return err
			}
			a.contaminated = true
		}
	}
	if a.contaminated {
		if err := g.RestoreReview(ctx, gitRun, a.snapshot); err != nil {
			return err
		}
	}
	_, err := s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		_, err := tx.ExecContext(ctx, "UPDATE review_attempts SET restored=1 WHERE attempt_id=?", a.id)
		return events.Draft{RunID: gitRun.ID, Type: "review.restored", Payload: map[string]any{"attempt": a.number, "contaminated": a.contaminated, "target_sha": a.target}}, err
	})
	if err == nil {
		a.restored = true
	}
	return err
}
func (s *Scheduler) failReview(ctx context.Context, run workflow.Run, a reviewAttempt, exit *int, report *review.Report, cause error) error {
	var data any
	if report != nil {
		encoded, err := json.Marshal(report)
		if err != nil {
			return err
		}
		data = string(encoded)
	}
	_, err := s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		if _, err := tx.ExecContext(ctx, "UPDATE review_attempts SET report_json=?,accepted=0 WHERE attempt_id=?", data, a.id); err != nil {
			return events.Draft{}, err
		}
		_, err := tx.ExecContext(ctx, `UPDATE phase_attempts SET status='failed',exit_code=?,error=?,ended_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, exit, cause.Error(), a.id)
		return events.Draft{RunID: run.ID, Type: "phase.failed", Payload: map[string]any{"phase": workflow.Review, "round": run.ReviewRound, "attempt": a.number, "target_sha": a.target, "result": report, "error": cause.Error()}}, err
	})
	return err
}
func (s *Scheduler) abortReview(ctx context.Context, repo config.Repository, run workflow.Run, gitRun managedgit.Run, a *reviewAttempt, cause error) error {
	if err := s.deps.Runner.StopSession(ctx, a.ref); err != nil {
		return err
	}
	if err := s.restoreReview(ctx, gitRun, a); err != nil {
		return s.recordAttention(ctx, repo, run, err)
	}
	// Early discovery failures obey the same attempt budget as completed reports.
	retry, err := s.retryPhase(ctx, run, workflow.Review, a.attempt, cause, repo.Reviewer.MaxAttempts)
	if err != nil {
		return err
	}
	if invalidSessionDiscovery(cause) && retry {
		return s.failReview(ctx, run, *a, nil, nil, cause)
	}
	return s.rejectReview(ctx, repo, run, *a, nil, nil, cause)
}
func reviewCanRetry(err error, number, max int) bool {
	var coded *fault.Error
	if errors.As(err, &coded) && coded.Code == "review.head_changed" {
		return false
	}
	return canRetryAttempt(err, number, max)
}
func reviewFailure(err error, number int) error {
	return &fault.Error{Code: errorCodeForReview(err), Message: fmt.Sprintf("Review attempt %d could not be accepted: %s", number, err.Error()), Err: err}
}
func errorCodeForReview(err error) string {
	var coded *fault.Error
	if errors.As(err, &coded) {
		return coded.Code
	}
	return "review.failed"
}

// Recovery may finish an interrupted restore in an attention run, but it never
// resumes automation or accepts its report.
func (s *Scheduler) restorePendingReview(ctx context.Context, run workflow.Run) error {
	a, err := s.lastReview(ctx, run)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !a.restoring || a.restored {
		return nil
	}
	status, err := s.deps.Runner.SessionStatus(ctx, a.ref)
	if err != nil {
		return err
	}
	if status.State == runner.SessionRunning {
		return &fault.Error{Code: "review.restore_pending", Message: "Review is still running; cannot restore repository safely"}
	}
	_, gitRun, err := s.context(ctx, run)
	if err != nil {
		return err
	}
	return s.restoreReview(ctx, gitRun, &a)
}

func (s *Scheduler) rejectReview(ctx context.Context, repo config.Repository, run workflow.Run, a reviewAttempt, exit *int, report *review.Report, cause error) error {
	failure := &fault.Error{Code: errorCodeForReview(cause), Message: cause.Error(), Err: cause}
	var coded *fault.Error
	if errors.As(cause, &coded) && coded.Message != "" {
		failure.Message = coded.Message
	}
	_, err := s.workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.OperationFailed, Failure: failure, Metadata: workflow.MetadataPatch{ReviewRejection: &workflow.ReviewRejection{AttemptID: a.id, Report: report, ExitCode: exit}}})
	if err != nil {
		return err
	}
	return s.attentionLabels(ctx, repo, run)
}
