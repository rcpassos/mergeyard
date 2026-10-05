package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/review"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/sessions"
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
	var input, snapshot string
	err := s.db.QueryRowContext(ctx, `SELECT a.id,a.attempt,a.status,COALESCE(a.process_session,''),a.input_path,a.resumed_session,COALESCE(a.error,''),v.session_id,v.target_sha,v.snapshot_json,v.restoration_started,v.restored,v.contaminated,a.agent FROM phase_attempts a JOIN review_attempts v ON v.attempt_id=a.id WHERE a.run_id=? AND a.phase='review' AND a.round=? ORDER BY a.attempt DESC LIMIT 1`, run.ID, run.ReviewRound).Scan(&a.id, &a.number, &a.status, &a.ref.Name, &input, &a.resumed, &a.failure, &a.sessionID, &a.target, &snapshot, &a.restoring, &a.restored, &a.contaminated, &a.agent)
	if err != nil {
		return a, err
	}
	a.ref.PhaseDir = filepath.Dir(input)
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
	if a.restoring && !a.restored {
		if err := s.restoreReview(ctx, gitRun, &a); err != nil {
			return s.recordAttention(ctx, repo, run, err)
		}
	}
	if a.status == "failed" {
		code, message, _ := strings.Cut(a.failure, ": ")
		cause := &fault.Error{Code: code, Message: message}
		if !reviewCanRetry(cause, a.number, repo.Reviewer.MaxAttempts) {
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
	if !reviewHeadMatches(pr, repo, run, gitRun, a.target) {
		return s.abortReview(ctx, repo, run, gitRun, &a, &fault.Error{Code: "review.head_changed", Message: "PR head changed during review; stale verdict discarded. Inspect preserved work before continuing"})
	}
	status, err := s.deps.Runner.SessionStatus(ctx, a.ref)
	if err != nil {
		return s.abortReview(ctx, repo, run, gitRun, &a, err)
	}

	if err := s.discoverSession(ctx, run.ID, workflow.Review, &a.attempt); err != nil {
		if !invalidSessionDiscovery(err) {
			return err
		}
		return s.abortReview(ctx, repo, run, gitRun, &a, err)
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
	var cause error
	if status.State != runner.SessionExited || status.ExitCode == nil {
		cause = &fault.Error{Code: "phase.session_missing", Message: "Review session has no exit metadata; inspect before retrying"}
	} else {
		stdout, err := s.deps.Runner.ReadFile(ctx, filepath.Join(a.ref.PhaseDir, "events.jsonl"))
		cause = err
		stderr, err := s.deps.Runner.ReadFile(ctx, filepath.Join(a.ref.PhaseDir, "stderr.log"))
		if cause == nil {
			cause = err
		}

		var lastMessage []byte
		if cause == nil && a.agent == "codex" {
			lastMessage, cause = s.readLastMessage(ctx, a.ref.PhaseDir)
		}
		if cause == nil {
			result, err := s.harnesses[a.agent].ParseResult(harness.PhaseContext{Phase: workflow.Review, SessionID: a.sessionID, Resume: a.resumed}, harness.PhaseArtifacts{LastMessage: lastMessage, Stdout: stdout, Stderr: stderr, ExitCode: *status.ExitCode})
			cause = err
			if err == nil {
				report = &review.Report{SchemaVersion: result.SchemaVersion, Status: result.Status, Summary: result.Summary, Findings: result.Findings}
				data, _ := json.Marshal(report)
				if err := s.deps.Runner.WriteFile(ctx, filepath.Join(a.ref.PhaseDir, "result.json"), data, 0600); err != nil {
					return err
				}
				if report.Status == "blocked" || report.Status == "failed" {
					cause = &fault.Error{Code: "phase." + report.Status, Message: report.Summary}
					if report.Summary == "" {
						cause = &fault.Error{Code: "phase." + report.Status, Message: "Reviewer reported " + report.Status}
					}
				}
			}
		}
	}
	if a.contaminated {
		cause = &fault.Error{Code: "review.code_changed", Message: "Reviewer changed repository files, index, or commits; restored pre-review work and discarded verdict"}
	}
	// Fetch again after restoration and parsing. Never authorize a different head.
	pr, err = s.deps.GitHub.GetPullRequest(ctx, repo.Repo, run.PRNumber)
	if err != nil {
		return err
	}
	if !reviewHeadMatches(pr, repo, run, gitRun, a.target) {
		cause = &fault.Error{Code: "review.head_changed", Message: "PR head changed during review; stale verdict discarded"}
	}
	if cause != nil {
		if !reviewCanRetry(cause, a.number, repo.Reviewer.MaxAttempts) {
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
		if run.ReviewRound >= s.cfg.MaxRounds {
			trigger = workflow.ReviewRoundsExhausted
			failure = &fault.Error{Code: "review.max_rounds_exceeded", Message: "Review requested changes with no review rounds remaining"}
		}
	}
	_, err = s.workflow.Transition(ctx, run.ID, workflow.Request{Trigger: trigger, Failure: failure, Metadata: workflow.MetadataPatch{ApprovedSHA: &approved, Review: &workflow.ReviewCompletion{AttemptID: a.id, Report: *report, ExitCode: *status.ExitCode}}})
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

	var sessionAgent string
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE(reviewer_agent,'') FROM runs WHERE id=?", run.ID).Scan(&sessionAgent); err != nil {
		return err
	}
	if err := validateRoleHarness("reviewer", sessionAgent, repo.Reviewer.Agent); err != nil {
		return fail(err)
	}
	adapter := s.harnesses[repo.Reviewer.Agent]
	g, ok := s.deps.Git.(ReviewGit)
	if !ok {
		return fail(&fault.Error{Code: "git.review_unsupported", Message: "Git adapter cannot protect reviewer changes"})
	}
	if run.ReviewRound > 1 {
		fixer, ok := s.deps.Git.(FixGit)
		if !ok || run.Fix == nil {
			return fail(&fault.Error{Code: "git.fix_unsupported", Message: "Cannot verify the pinned fix before re-review"})
		}
		if err := fixer.InspectFixTarget(ctx, gitRun, run.Fix.CommitSHA); err != nil {
			return fail(err)
		}
	}
	snapshot, err := g.SnapshotReview(ctx, gitRun)
	if err != nil {
		return fail(err)
	}
	pr, err := s.deps.GitHub.GetPullRequest(ctx, repo.Repo, run.PRNumber)
	if err != nil {
		return err
	}
	if !reviewHeadMatches(pr, repo, run, gitRun, snapshot.Head) {
		return fail(&fault.Error{Code: "review.head_changed", Message: "Local review target differs from the PR head; inspect preserved work"})
	}
	diff, err := g.ReviewDiff(ctx, gitRun, snapshot.Head)
	if err != nil {
		return fail(err)
	}
	phaseDir := filepath.Join(s.workspace.Root, "runs", run.ID, "phases", fmt.Sprintf("review-%d-%d", run.ReviewRound, number))
	if err := os.MkdirAll(phaseDir, 0700); err != nil {
		return fail(err)
	}
	var sessionID string
	err = s.db.QueryRowContext(ctx, "SELECT COALESCE(reviewer_session_id,'') FROM runs WHERE id=?", run.ID).Scan(&sessionID)
	if err != nil {
		return err
	}
	resume := number > 1 || run.ReviewRound > 1
	warning := ""
	previousSession := sessionID
	if number > 1 {
		previous, err := s.lastReview(ctx, run)
		if err != nil {
			return err
		}
		if strings.HasPrefix(previous.failure, "harness.session_resume_failed: ") {
			sessionID = ""
			resume = false
			warning = "harness.session_resume_failed"
		}
	}
	if sessionID == "" {
		if adapter.Capabilities().SessionIDSource == harness.Preassigned {
			sessionID = uuid.NewString()
		}
		resume = false
	}
	phase := harness.PhaseContext{Phase: workflow.Review, WorktreePath: gitRun.Path, PhaseDir: phaseDir, SessionID: sessionID, Resume: resume, Env: s.deps.Env}
	issue, err = s.deps.GitHub.GetIssue(ctx, repo.Repo, run.IssueNumber)
	if err != nil {
		return err
	}
	var previousFindings []review.Finding
	var fixReport *review.FixReport
	if run.ReviewRound > 1 {
		if run.Fix == nil || !run.Fix.Pushed || run.Fix.CommitSHA != snapshot.Head || run.Fix.Round != run.ReviewRound-1 {
			return fail(&fault.Error{Code: "review.head_changed", Message: "Next review target differs from the pinned fix commit"})
		}
		previousFindings, fixReport = run.Fix.Findings, run.Fix.Report
	}
	input, err := harness.WriteReviewInput(ctx, s.deps.Runner, phase, harness.ReviewInput{Issue: harness.ImplementInput{Repository: repo.Repo, IssueNumber: issue.Number, IssueTitle: issue.Title, IssueBody: issue.Body, IssueURL: issue.URL, BaseBranch: gitRun.BaseBranch}, PRNumber: pr.Number, PRURL: pr.URL, TargetSHA: snapshot.Head, BaseSHA: gitRun.BaseSHA, Diff: diff, Round: run.ReviewRound, PRBody: pr.Body, PreviousFindings: previousFindings, FixReport: fixReport})
	if err != nil {
		return fail(err)
	}
	command, err := s.harnesses[repo.Reviewer.Agent].BuildInvocation(phase, repo.Reviewer)
	if err != nil {
		return fail(err)
	}
	req := runner.SessionRequest{RunID: run.ID, Phase: "review", Round: run.ReviewRound, Attempt: number, PhaseDir: phaseDir, Command: command}
	id := uuid.NewString()
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	skills, _ := json.Marshal(repo.Reviewer.Skills)
	permission, allowedTools := s.roleSettings(repo.Reviewer.Agent)
	tools, _ := json.Marshal(allowedTools)
	_, err = s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		if _, err := tx.ExecContext(ctx, "UPDATE runs SET reviewer_agent=?,reviewer_session_id=? WHERE id=?", repo.Reviewer.Agent, sessionID, run.ID); err != nil {
			return events.Draft{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,model,effort,status,resumed_session,process_session,input_path,result_path,log_path,skills_json,permissions) VALUES (?,?,'review','reviewer',?,?,?,?,?,'running',?,?,?,?,?,?,?)`, id, run.ID, run.ReviewRound, number, repo.Reviewer.Agent, repo.Reviewer.Model, repo.Reviewer.Effort, phase.Resume, sessions.Name(req), input, filepath.Join(phaseDir, "result.json"), filepath.Join(phaseDir, "events.jsonl"), string(skills), s.rolePermissions(repo.Reviewer.Agent)); err != nil {
			return events.Draft{}, err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO review_attempts(attempt_id,target_sha,diff,snapshot_json,session_id,permission_mode,allowed_tools_json) VALUES (?,?,?,?,?,?,?)`, id, snapshot.Head, diff, string(snapshotJSON), sessionID, permission, string(tools))
		return events.Draft{RunID: run.ID, Type: "phase.attempt_started", Payload: map[string]any{"agent": repo.Reviewer.Agent, "resumed_session": resume, "permissions": s.rolePermissions(repo.Reviewer.Agent), "phase": workflow.Review, "round": run.ReviewRound, "attempt": number, "target_sha": snapshot.Head, "session_id": sessionID, "model": repo.Reviewer.Model, "effort": repo.Reviewer.Effort, "skills": repo.Reviewer.Skills, "permission_mode": permission, "allowed_tools": allowedTools, "warning_code": warning, "previous_session_id": previousSession}}, err
	})
	if err != nil {
		return err
	}
	ref, err := s.deps.Runner.StartSession(ctx, req)
	if err != nil && ref.Name != "" && ctx.Err() == nil {
		return nil
	}
	if err != nil && ctx.Err() == nil {
		a := reviewAttempt{attempt: attempt{id: id, agent: repo.Reviewer.Agent, number: number}, target: snapshot.Head, snapshot: snapshot}
		if restoreErr := s.restoreReview(ctx, gitRun, &a); restoreErr != nil {
			return fail(restoreErr)
		}
		if !reviewCanRetry(err, number, repo.Reviewer.MaxAttempts) {
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
