package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/sessions"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func (s *Scheduler) advance(ctx context.Context, repo config.Repository, run workflow.Run) error {
	return s.workflow.WithRunOperation(ctx, run.ID, func(operationContext context.Context, current workflow.Run) error {
		return s.advanceRun(operationContext, repo, current)
	})
}
func (s *Scheduler) advanceRun(ctx context.Context, repo config.Repository, run workflow.Run) error {
	if run.State.Terminal() {
		return nil
	}
	var stopRequested bool
	if err := s.db.QueryRowContext(ctx, "SELECT stop_requested FROM runs WHERE id=?", run.ID).Scan(&stopRequested); err != nil {
		return err
	}
	if stopRequested {
		return s.stopRun(ctx, run)
	}
	if run.State == workflow.NeedsAttention {
		return s.attentionLabels(ctx, repo, run)
	}
	if run.State == workflow.Active && run.Phase == workflow.Fix {
		return s.fix(ctx, repo, run)
	}
	if run.State == workflow.Active && run.Phase == workflow.Review {
		return s.review(ctx, repo, run)
	}
	if run.State != workflow.Claiming && run.State != workflow.Preparing && !(run.State == workflow.Active && run.Phase == workflow.Implement) {
		return nil
	}
	issue, gitRun, err := s.context(ctx, run)
	if errors.Is(err, sql.ErrNoRows) && run.State == workflow.Claiming {
		var issues []github.Issue
		issues, err = s.deps.GitHub.ListOpenIssues(ctx, repo.Repo)
		if err == nil {
			err = &fault.Error{Code: "github.issue_closed", Message: "Claimed issue is no longer open"}
			for _, candidate := range issues {
				if candidate.Number == run.IssueNumber && candidate.State == github.Open {
					issue = candidate
					err = s.saveIssue(ctx, run.ID, issue)
					break
				}
			}
		}
	}
	if err != nil {
		return s.attention(ctx, repo, run, err)
	}
	if run.State == workflow.Claiming {
		// PRD §7.3: durable CLAIMING, add running, remove ready, persist success.
		if err := s.deps.GitHub.AddLabel(ctx, repo.Repo, run.IssueNumber, repo.Labels.Running); err != nil {
			return s.attention(ctx, repo, run, err)
		}
		if err := s.deps.GitHub.RemoveLabel(ctx, repo.Repo, run.IssueNumber, repo.Labels.Ready); err != nil {
			return s.attention(ctx, repo, run, err)
		}
		run, err = s.workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.ClaimSucceeded})
		if err != nil {
			return err
		}
	}
	if run.State == workflow.Preparing {
		request := managedgit.PrepareRequest{Repository: repo.Repo, BaseBranch: repo.BaseBranch, RunID: run.ID, IssueNumber: run.IssueNumber}
		if gitRun.ID != "" {
			request.KnownRuns = []managedgit.Run{gitRun}
		}
		gitRun, err = s.deps.Git.Prepare(ctx, request)
		if err != nil {
			return s.attention(ctx, repo, run, err)
		}
		if err := s.saveGit(ctx, run.ID, gitRun, repo.Implementer.Agent); err != nil {
			return s.attention(ctx, repo, run, err)
		}
		run, err = s.workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.WorktreeReady})
		if err != nil {
			return err
		}
	}
	if gitRun.ID == "" {
		return s.attention(ctx, repo, run, &fault.Error{Code: "git.ownership_missing", Message: "Run has no persisted Git ownership"})
	}
	result, done, err := s.implement(ctx, repo, run, issue, gitRun)
	if err != nil {
		return s.attention(ctx, repo, run, err)
	}
	if !done {
		return nil
	}
	if _, err := s.deps.Git.CommitAndPush(ctx, gitRun, managedgit.Phase{Title: issue.Title, RequireChanges: true}); err != nil {
		return s.attention(ctx, repo, run, err)
	}
	pr, err := s.deps.GitHub.FindOpenPullRequest(ctx, repo.Repo, gitRun.Branch)
	if err != nil {
		return s.attention(ctx, repo, run, err)
	}
	if pr == nil {
		pr, err = s.deps.GitHub.CreateDraftPullRequest(ctx, repo.Repo, gitRun.Branch, gitRun.BaseBranch, github.PullRequestContent{IssueNumber: issue.Number, IssueTitle: issue.Title, Summary: result.Summary, RunID: run.ID})
		if err != nil {
			return s.attention(ctx, repo, run, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE scheduler_runs SET pr_url=? WHERE run_id=?", pr.URL, run.ID); err != nil {
		return err
	}
	round := 1
	_, err = s.workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.ImplementSucceeded, Metadata: workflow.MetadataPatch{PRNumber: &pr.Number, ReviewRound: &round}})
	// The next reconciliation tick continues this durable endpoint into review.
	return err
}

func (s *Scheduler) attention(ctx context.Context, repo config.Repository, run workflow.Run, cause error) error {
	return s.workflow.WithRunOperation(ctx, run.ID, func(operationContext context.Context, current workflow.Run) error {
		if current.State == workflow.Manual || current.State.Terminal() {
			return nil
		}
		return s.recordAttention(operationContext, repo, current, cause)
	})
}
func (s *Scheduler) recordAttention(ctx context.Context, repo config.Repository, run workflow.Run, cause error) error {
	// A cancelled tick leaves the persisted work and tmux session for the next tick.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	failure := &fault.Error{Code: "internal.scheduler", Message: cause.Error(), Err: cause}
	var coded *fault.Error
	if errors.As(cause, &coded) {
		failure.Code = coded.Code
		if coded.Message != "" {
			failure.Message = coded.Message
		}
	}
	if _, err := s.workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.OperationFailed, Failure: failure}); err != nil {
		return err
	}
	return s.attentionLabels(ctx, repo, run)
}
func (s *Scheduler) attentionLabels(ctx context.Context, repo config.Repository, run workflow.Run) error {
	if err := s.deps.GitHub.RemoveLabel(ctx, repo.Repo, run.IssueNumber, repo.Labels.Running); err != nil {
		return err
	}
	return s.deps.GitHub.AddLabel(ctx, repo.Repo, run.IssueNumber, repo.Labels.NeedsAttention)
}

func (s *Scheduler) saveIssue(ctx context.Context, id string, issue github.Issue) error {
	data, err := json.Marshal(issue)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO scheduler_runs (run_id,issue_json) VALUES (?,?)", id, string(data))
	return err
}
func (s *Scheduler) saveGit(ctx context.Context, id string, run managedgit.Run, agent string) error {
	data, err := json.Marshal(run)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE scheduler_runs SET git_json=? WHERE run_id=?", string(data), id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE runs SET branch=?, worktree_path=?, implementer_agent=? WHERE id=?", run.Branch, run.Path, agent, id); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Scheduler) context(ctx context.Context, run workflow.Run) (github.Issue, managedgit.Run, error) {
	var issue github.Issue
	var gitRun managedgit.Run
	var issueJSON string
	var gitJSON sql.NullString
	if err := s.db.QueryRowContext(ctx, "SELECT issue_json,git_json FROM scheduler_runs WHERE run_id=?", run.ID).Scan(&issueJSON, &gitJSON); err != nil {
		return issue, gitRun, err
	}
	if err := json.Unmarshal([]byte(issueJSON), &issue); err != nil {
		return issue, gitRun, err
	}
	if gitJSON.Valid {
		if err := json.Unmarshal([]byte(gitJSON.String), &gitRun); err != nil {
			return issue, gitRun, err
		}
	}
	return issue, gitRun, nil
}

type attempt struct {
	id        string
	agent     string
	number    int
	status    string
	ref       runner.SessionRef
	sessionID string
	resumed   bool
	failure   string
}

func (s *Scheduler) lastAttempt(ctx context.Context, id string) (attempt, error) {
	var a attempt
	var input string
	var savedSession sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id,attempt,status,COALESCE(process_session,''),COALESCE(input_path,''),resumed_session,COALESCE(error,''),agent,session_id
 FROM phase_attempts WHERE run_id=? AND phase='implement' ORDER BY attempt DESC LIMIT 1`, id).Scan(&a.id, &a.number, &a.status, &a.ref.Name, &input, &a.resumed, &a.failure, &a.agent, &savedSession)
	if err != nil {
		return a, err
	}
	a.ref.PhaseDir = filepath.Dir(input)
	err = s.db.QueryRowContext(ctx, "SELECT COALESCE(implementer_session_id,'') FROM runs WHERE id=?", id).Scan(&a.sessionID)
	if savedSession.Valid {
		a.sessionID = savedSession.String
	}
	return a, err
}

func (s *Scheduler) implement(ctx context.Context, repo config.Repository, run workflow.Run, issue github.Issue, gitRun managedgit.Run) (harness.PhaseResult, bool, error) {
	a, err := s.lastAttempt(ctx, run.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return harness.PhaseResult{}, false, s.startAttempt(ctx, repo, run, issue, gitRun, 1)
	}
	if err != nil {
		return harness.PhaseResult{}, false, err
	}
	if a.status == "succeeded" {
		data, err := s.deps.Runner.ReadFile(ctx, filepath.Join(a.ref.PhaseDir, "result.json"))
		if err != nil {
			return harness.PhaseResult{}, false, err
		}
		var result harness.PhaseResult
		err = json.Unmarshal(data, &result)
		return result, err == nil, err
	}
	if a.status == "failed" {
		max := repo.Implementer.MaxAttempts
		if max < 1 {
			max = 1
		}
		code, message, _ := strings.Cut(a.failure, ": ")
		cause := &fault.Error{Code: code, Message: message}
		retry, err := s.retryPhase(ctx, run, workflow.Implement, a, cause, max)
		if err != nil {
			return harness.PhaseResult{}, false, err
		}
		if !retry {
			return harness.PhaseResult{}, false, attemptFailure(cause, a.number, max)
		}
		return harness.PhaseResult{}, false, s.startAttempt(ctx, repo, run, issue, gitRun, a.number+1)
	}
	status, err := s.deps.Runner.SessionStatus(ctx, a.ref)
	if err != nil {
		return harness.PhaseResult{}, false, err
	}
	discoveryErr := s.discoverImplementSession(ctx, run.ID, &a)
	if discoveryErr != nil {
		if !invalidSessionDiscovery(discoveryErr) {
			return harness.PhaseResult{}, false, discoveryErr
		}
		if status.State == runner.SessionRunning {
			// An invalid identity cannot safely be resumed. Stop this owned attempt
			// and persist its failure through the same completion path as an exit.
			if err := s.deps.Runner.StopSession(ctx, a.ref); err != nil {
				return harness.PhaseResult{}, false, err
			}
			status, err = s.deps.Runner.SessionStatus(ctx, a.ref)
			if err != nil {
				return harness.PhaseResult{}, false, err
			}
		}
	}
	if status.State == runner.SessionRunning {
		return harness.PhaseResult{}, false, nil
	}
	var artifacts harness.PhaseArtifacts
	err = discoveryErr
	if err == nil && (status.State != runner.SessionExited || status.ExitCode == nil) {
		err = &fault.Error{Code: "phase.session_missing", Message: "Implement session has no exit metadata; inspect before retrying"}
	} else if err == nil {
		artifacts.ExitCode = *status.ExitCode
		artifacts.Stdout, err = s.deps.Runner.ReadFile(ctx, filepath.Join(a.ref.PhaseDir, "events.jsonl"))
		if err == nil {
			artifacts.Stderr, err = s.deps.Runner.ReadFile(ctx, filepath.Join(a.ref.PhaseDir, "stderr.log"))
		}
	}
	if err == nil && a.agent == "codex" {
		artifacts.LastMessage, err = s.deps.Runner.ReadFile(ctx, filepath.Join(a.ref.PhaseDir, "last-message.json"))
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		} // ParseResult reports the native missing-result diagnostic.
	}
	var result harness.PhaseResult
	if err == nil {
		result, err = s.harnesses[a.agent].ParseResult(harness.PhaseContext{Phase: workflow.Implement, WorktreePath: gitRun.Path, PhaseDir: a.ref.PhaseDir, SessionID: a.sessionID, Resume: a.resumed}, artifacts)
	}
	if err == nil {
		data, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			return result, false, marshalErr
		}
		if err := s.deps.Runner.WriteFile(ctx, filepath.Join(a.ref.PhaseDir, "result.json"), data, 0600); err != nil {
			return result, false, err
		}
		if result.Status == "blocked" {
			err = &fault.Error{Code: "phase.blocked", Message: result.Summary}
		} else if result.Status == "failed" {
			err = &fault.Error{Code: "phase.failed", Message: result.Summary}
		}
	}
	if ctx.Err() != nil {
		return result, false, ctx.Err()
	}
	attemptStatus, event := "succeeded", "phase.completed"
	if err != nil {
		attemptStatus, event = "failed", "phase.failed"
	}
	failureText := ""
	if err != nil {
		failureText = err.Error()
	}
	_, saveErr := s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		_, saveErr := tx.ExecContext(ctx, `UPDATE phase_attempts SET status=?,exit_code=?,ended_at=strftime('%Y-%m-%dT%H:%M:%fZ','now'),error=? WHERE id=?`, attemptStatus, status.ExitCode, failureText, a.id)
		return events.Draft{RunID: run.ID, Type: event, Payload: map[string]any{"attempt": a.number, "result": result, "error": failureText}}, saveErr
	})
	if saveErr != nil {
		return result, false, saveErr
	}
	if err != nil {
		retry, retryErr := s.retryPhase(ctx, run, workflow.Implement, a, err, repo.Implementer.MaxAttempts)
		if retryErr != nil {
			return result, false, retryErr
		}
		if retry {
			return result, false, nil
		}
		return result, false, attemptFailure(err, a.number, repo.Implementer.MaxAttempts)
	}
	return result, true, nil
}

func (s *Scheduler) startAttempt(ctx context.Context, repo config.Repository, run workflow.Run, issue github.Issue, gitRun managedgit.Run, number int) error {
	var sessionID, sessionAgent string
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE(implementer_session_id,''),COALESCE(implementer_agent,'') FROM runs WHERE id=?", run.ID).Scan(&sessionID, &sessionAgent); err != nil {
		return err
	}
	if err := validateImplementerHarness(sessionAgent, repo.Implementer.Agent); err != nil {
		return err
	}
	adapter := s.harnesses[repo.Implementer.Agent]
	resume := sessionID != ""
	var previous attempt
	recovery := false
	if number > 1 {
		var err error
		previous, err = s.lastAttempt(ctx, run.ID)
		if err != nil {
			return err
		}
		recovery = strings.HasPrefix(previous.failure, "harness.session_resume_failed: ")
		if recovery {
			sessionID, resume = "", false
		}
	}
	if adapter.Capabilities().SessionIDSource == harness.Preassigned && sessionID == "" {
		sessionID = uuid.NewString()
	}
	phaseDir := filepath.Join(s.workspace.Root, "runs", run.ID, "phases", fmt.Sprintf("implement-0-%d", number))
	if err := os.MkdirAll(phaseDir, 0700); err != nil {
		return err
	}
	phase := harness.PhaseContext{Phase: workflow.Implement, WorktreePath: gitRun.Path, PhaseDir: phaseDir, SessionID: sessionID, Resume: resume, Env: s.deps.Env}
	issue, err := s.deps.GitHub.GetIssue(ctx, repo.Repo, run.IssueNumber)
	if err != nil {
		return err
	}
	input, err := harness.WriteImplementInput(ctx, s.deps.Runner, phase, harness.ImplementInput{Repository: repo.Repo, IssueNumber: issue.Number, IssueTitle: issue.Title, IssueBody: issue.Body, IssueURL: issue.URL, BaseBranch: gitRun.BaseBranch})
	if err != nil {
		return err
	}
	command, err := s.harnesses[repo.Implementer.Agent].BuildInvocation(phase, repo.Implementer)
	if err != nil {
		return err
	}
	req := runner.SessionRequest{RunID: run.ID, Phase: "implement", Round: 0, Attempt: number, PhaseDir: phaseDir, Command: command}
	// Persist the deterministic process identity before launch; an ambiguous start
	// must be observed on the next tick, never replayed as a new process.
	skills, err := json.Marshal(repo.Implementer.Skills)
	if err != nil {
		return err
	}
	id := uuid.NewString()
	_, err = s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		if _, err := tx.ExecContext(ctx, "UPDATE runs SET implementer_session_id=? WHERE id=?", sessionID, run.ID); err != nil {
			return events.Draft{}, err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO phase_attempts (id,run_id,phase,role,round,attempt,agent,model,effort,status,resumed_session,process_session,input_path,result_path,log_path,skills_json,permissions,session_id)
 VALUES (?,?,'implement','implementer',0,?,?,?,?,'running',?,?,?,?,?,?,?,?)`, id, run.ID, number, repo.Implementer.Agent, repo.Implementer.Model, repo.Implementer.Effort, phase.Resume, sessions.Name(req), input, filepath.Join(phaseDir, "result.json"), filepath.Join(phaseDir, "events.jsonl"), string(skills), s.rolePermissions(repo.Implementer.Agent), sessionID)
		if err != nil {
			return events.Draft{}, err
		}
		if recovery {
			return reserveRecovery(ctx, tx, run, workflow.Implement, previous, id, sessionID)
		}
		return events.Draft{RunID: run.ID, Type: "phase.attempt_started", Payload: map[string]any{
			"phase": workflow.Implement, "round": 0, "attempt": number,
		}}, err
	})
	if err != nil {
		return err
	}
	ref, err := s.deps.Runner.StartSession(ctx, req)
	if err != nil && ref.Name != "" && ctx.Err() == nil {
		// Launch may have committed. Preserve running status for observation.
		return nil
	}
	if err != nil && ctx.Err() == nil {
		if _, saveErr := s.db.ExecContext(ctx, `UPDATE phase_attempts SET status='failed',ended_at=strftime('%Y-%m-%dT%H:%M:%fZ','now'),error=? WHERE run_id=? AND phase='implement' AND attempt=?`, err.Error(), run.ID, number); saveErr != nil {
			return saveErr
		}
		retry, retryErr := s.retryPhase(ctx, run, workflow.Implement, attempt{id: id, number: number, resumed: resume}, err, repo.Implementer.MaxAttempts)
		if retryErr != nil {
			return retryErr
		}
		if retry {
			return nil
		}
		return attemptFailure(err, number, repo.Implementer.MaxAttempts)
	}
	return err
}

func attemptFailure(err error, number, max int) error {
	var coded *fault.Error
	if errors.As(err, &coded) && (coded.Code == "phase.blocked" || coded.Code == "phase.result_invalid" || coded.Code == "phase.result_missing" || coded.Code == "phase.session_missing" || coded.Code == "harness.session_resume_failed" || coded.Code == "harness.session_identity_invalid") {
		return err
	}
	if number >= max {
		return &fault.Error{Code: "phase.retries_exhausted", Message: "Implement attempts exhausted: " + err.Error(), Err: err}
	}
	return err
}

func canRetryAttempt(err error, number, max int) bool {
	if max < 1 {
		max = 1
	}
	var coded *fault.Error
	if errors.As(err, &coded) && (coded.Code == "phase.blocked" || coded.Code == "phase.session_missing" || coded.Code == "harness.session_identity_invalid") {
		return false
	}
	return number < max
}

func canRetryImplementAttempt(err error, number, max int) bool {
	var coded *fault.Error
	if errors.As(err, &coded) && (coded.Code == "harness.session_resume_failed" || coded.Code == "harness.session_identity_invalid") {
		return false
	}
	return canRetryAttempt(err, number, max)
}

func (s *Scheduler) rolePermissions(agent string) string {
	if agent == "codex" {
		networkAccess := s.cfg.Agents.Codex.NetworkAccess
		if s.cfg.Agents.Codex.Sandbox == "danger-full-access" {
			// The workspace-write network toggle does not restrict full access.
			networkAccess = true
		}
		return s.cfg.Agents.Codex.Sandbox + " · network " + strconv.FormatBool(networkAccess) + " · approvals never"
	}
	return s.cfg.Agents.Claude.PermissionMode + " · allowed tools " + strings.Join(s.cfg.Agents.Claude.AllowedTools, ", ")
}

func validateImplementerHarness(persistedAgent, configuredAgent string) error {
	return validateRoleHarness("implementer", persistedAgent, configuredAgent)
}

func validateRoleHarness(role, persistedAgent, configuredAgent string) error {
	if persistedAgent != "" && persistedAgent != configuredAgent {
		return &fault.Error{Code: "harness.session_agent_changed", Message: "Persisted " + role + " belongs to " + persistedAgent + "; restore that role harness before resuming"}
	}
	return nil
}

func (s *Scheduler) roleSettings(agent string) (string, []string) {
	if agent == "codex" {
		return s.rolePermissions(agent), []string{}
	}
	permission := s.cfg.Agents.Claude.PermissionMode
	if permission == "" {
		permission = "bypassPermissions"
	}
	return permission, s.cfg.Agents.Claude.AllowedTools
}

func (s *Scheduler) readLastMessage(ctx context.Context, phaseDir string) ([]byte, error) {
	data, err := s.deps.Runner.ReadFile(ctx, filepath.Join(phaseDir, "last-message.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return data, err
}
