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
	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/review"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/sessions"
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
	var input, findings string
	var report, diagnostics sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT a.id,a.attempt,a.status,COALESCE(a.process_session,''),a.input_path,a.resumed_session,COALESCE(a.error,''),f.session_id,f.target_sha,COALESCE(f.commit_sha,''),f.pushed,f.findings_json,f.report_json,a.agent,f.ci_json FROM phase_attempts a JOIN fix_attempts f ON f.attempt_id=a.id WHERE a.run_id=? AND a.phase='fix' AND a.round=? ORDER BY a.attempt DESC LIMIT 1`, run.ID, run.ReviewRound).Scan(&a.id, &a.number, &a.status, &a.ref.Name, &input, &a.resumed, &a.failure, &a.sessionID, &a.target, &a.commit, &a.pushed, &findings, &report, &a.agent, &diagnostics)
	if err != nil {
		return a, err
	}
	a.ref.PhaseDir = filepath.Dir(input)
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
	if run.ReviewRound >= s.cfg.MaxRounds {
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
	if a.status == "failed" {
		code, message, _ := strings.Cut(a.failure, ": ")
		cause := &fault.Error{Code: code, Message: message}
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
		status, err := s.deps.Runner.SessionStatus(ctx, a.ref)
		if err != nil {
			return fail(err)
		}

		if err := s.discoverSession(ctx, run.ID, workflow.Fix, &a.attempt); err != nil {
			if !invalidSessionDiscovery(err) {
				return err
			}
			if err := s.deps.Runner.StopSession(ctx, a.ref); err != nil {
				return err
			}
			return s.finishFixAttempt(ctx, repo, run, a, nil, nil, err)
		}
		if status.State == runner.SessionRunning {
			return nil
		}

		var cause error
		var report *review.FixReport
		if status.State != runner.SessionExited || status.ExitCode == nil {
			cause = &fault.Error{Code: "phase.session_missing", Message: "Fix session has no exit metadata; inspect before retrying"}
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
				result, err := s.harnesses[a.agent].ParseResult(harness.PhaseContext{Phase: workflow.Fix, SessionID: a.sessionID, Resume: a.resumed}, harness.PhaseArtifacts{LastMessage: lastMessage, Stdout: stdout, Stderr: stderr, ExitCode: *status.ExitCode})
				cause = err
				if err == nil {
					report = &review.FixReport{SchemaVersion: result.SchemaVersion, Status: result.Status, Summary: result.Summary, Responses: result.Responses}
					if err := report.ValidateFindings(a.findings); err != nil {
						cause = &fault.Error{Code: "phase.result_invalid", Message: "Fix responses do not match supplied findings", Err: err}
						report = nil
					} else {
						data, _ := json.Marshal(report)
						if err := s.deps.Runner.WriteFile(ctx, filepath.Join(a.ref.PhaseDir, "result.json"), data, 0600); err != nil {
							return err
						}
						if report.Status != "success" {
							cause = &fault.Error{Code: "phase." + report.Status, Message: report.Summary}
						}
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
		if err != nil {
			return fail(err)
		}
		if !result.TreeChanged {
			if a.ci != nil {
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

func (s *Scheduler) startFix(ctx context.Context, repo config.Repository, run workflow.Run, gitRun managedgit.Run, number int) error {
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
		if (cause == "ci.check_failed" || cause == "ci.check_timed_out") && run.CI.RepairCause == cause {
			repair = run.CI
		}
	}
	if run.Review.Report.Status != "changes_required" && repair == nil {
		return fail(&fault.Error{Code: "phase.result_invalid", Message: "Fix requires blocking findings or pinned repairable CI evidence"})
	}
	issue, err := s.deps.GitHub.GetIssue(ctx, repo.Repo, run.IssueNumber)
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
	findings := []review.Finding{}
	for _, f := range run.Review.Report.Findings {
		if f.Severity == "blocking" {
			findings = append(findings, f)
		}
	}
	var sessionID string
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE(implementer_session_id,'') FROM runs WHERE id=?", run.ID).Scan(&sessionID); err != nil {
		return err
	}
	if sessionID == "" && number == 1 {
		return fail(&fault.Error{Code: "harness.session_missing", Message: "Original implementer session is unavailable; inspect before fixing"})
	}
	adapter := s.harnesses[repo.Implementer.Agent]
	resume := sessionID != ""
	warning, previousSession := "", sessionID
	var previous fixAttempt
	if number > 1 {
		previous, err = s.lastFix(ctx, run)
		if err != nil {
			return err
		}

		// A discovered-identity fresh attempt can fail before starting a thread.
		// Continue that authorized execution within its remaining attempt budget;
		// missing identity in an established conversation still requires inspection.
		if sessionID == "" && (previous.resumed || previous.sessionID != "" || adapter.Capabilities().SessionIDSource != harness.Discovered) {
			return fail(&fault.Error{Code: "harness.session_missing", Message: "Persisted fix conversation is unavailable; inspect before retrying"})
		}
		if strings.HasPrefix(previous.failure, "harness.session_resume_failed: ") {
			sessionID = ""
			if adapter.Capabilities().SessionIDSource == harness.Preassigned {
				sessionID = uuid.NewString()
			}
			resume = false
			warning = "harness.session_resume_failed"
		}
	}
	phaseDir := filepath.Join(s.workspace.Root, "runs", run.ID, "phases", fmt.Sprintf("fix-%d-%d", run.ReviewRound, number))
	if err := os.MkdirAll(phaseDir, 0700); err != nil {
		return fail(err)
	}
	if err := sessions.RequireProcessJournal(phaseDir); err != nil {
		return fail(err)
	}
	phase := harness.PhaseContext{Phase: workflow.Fix, WorktreePath: gitRun.Path, PhaseDir: phaseDir, SessionID: sessionID, Resume: resume, Env: s.deps.Env}
	input, err := harness.WriteFixInput(ctx, s.deps.Runner, phase, harness.FixInput{Issue: harness.ImplementInput{Repository: repo.Repo, IssueNumber: issue.Number, IssueTitle: issue.Title, IssueBody: issue.Body, IssueURL: issue.URL, BaseBranch: gitRun.BaseBranch}, PRNumber: pr.Number, PRURL: pr.URL, PRBody: pr.Body, TargetSHA: target, Round: run.ReviewRound, Findings: findings, CI: repair})
	if err != nil {
		return fail(err)
	}
	command, err := s.harnesses[repo.Implementer.Agent].BuildInvocation(phase, repo.Implementer)
	if err != nil {
		return fail(err)
	}
	req := runner.SessionRequest{RunID: run.ID, Phase: "fix", Round: run.ReviewRound, Attempt: number, PhaseDir: phaseDir, Command: command}
	id := uuid.NewString()
	skills, _ := json.Marshal(repo.Implementer.Skills)
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
	_, err = s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		if _, err := tx.ExecContext(ctx, "UPDATE runs SET implementer_session_id=? WHERE id=?", sessionID, run.ID); err != nil {
			return events.Draft{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,model,effort,status,resumed_session,process_session,input_path,result_path,log_path,skills_json,permissions) VALUES (?,?,'fix','implementer',?,?,?,?,?,'running',?,?,?,?,?,?,?)`, id, run.ID, run.ReviewRound, number, repo.Implementer.Agent, repo.Implementer.Model, repo.Implementer.Effort, resume, sessions.Name(req), input, filepath.Join(phaseDir, "result.json"), filepath.Join(phaseDir, "events.jsonl"), string(skills), s.rolePermissions(repo.Implementer.Agent)); err != nil {
			return events.Draft{}, err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO fix_attempts(attempt_id,session_id,target_sha,findings_json,permission_mode,allowed_tools_json,ci_json) VALUES (?,?,?,?,?,?,?)`, id, sessionID, target, string(encoded), permission, string(tools), diagnostics)
		if err != nil {
			return events.Draft{}, err
		}
		if warning != "" {
			return reserveRecovery(ctx, tx, run, workflow.Fix, previous.attempt, id, sessionID)
		}
		return events.Draft{RunID: run.ID, Type: "phase.attempt_started", Payload: map[string]any{"agent": repo.Implementer.Agent, "permissions": s.rolePermissions(repo.Implementer.Agent), "phase": workflow.Fix, "round": run.ReviewRound, "attempt": number, "session_id": sessionID, "resumed_session": resume, "model": repo.Implementer.Model, "effort": repo.Implementer.Effort, "skills": repo.Implementer.Skills, "permission_mode": permission, "allowed_tools": allowedTools, "warning_code": warning, "previous_session_id": previousSession, "ci": repair}}, err
	})
	if err != nil {
		return err
	}
	ref, err := s.deps.Runner.StartSession(ctx, req)
	if err != nil && ref.Name != "" && ctx.Err() == nil {
		return nil
	}
	if err != nil && ctx.Err() == nil {
		return s.finishFixAttempt(ctx, repo, run, fixAttempt{attempt: attempt{id: id, agent: repo.Implementer.Agent, number: number}}, nil, nil, err)
	}
	return err
}
