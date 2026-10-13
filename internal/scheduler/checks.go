package scheduler

import (
	"context"
	"database/sql"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/workflow"
	"strings"
	"time"
)

type ImplementGit interface {
	CommitImplement(context.Context, managedgit.Run, managedgit.Phase) (managedgit.CommitResult, error)
	PushImplement(context.Context, managedgit.Run, string) error
}

func (s *Scheduler) checkImplementation(ctx context.Context, repo config.Repository, run workflow.Run, issue github.Issue, gitRun managedgit.Run) (bool, error) {
	a, err := s.lastAttempt(ctx, run.ID)
	if err != nil {
		return false, err
	}
	g, ok := s.deps.Git.(ImplementGit)
	if !ok {
		return false, &fault.Error{Code: "git.implement_unsupported", Message: "Git adapter cannot gate publication on checks"}
	}
	var commit string
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE(commit_sha,'') FROM phase_attempts WHERE id=?", a.id).Scan(&commit); err != nil {
		return false, err
	}
	if commit == "" {
		result, err := g.CommitImplement(ctx, gitRun, managedgit.Phase{Title: issue.Title, RequireChanges: true})
		if diagnosticErr := s.recordStagingDiagnostics(ctx, run, result); diagnosticErr != nil {
			return false, diagnosticErr
		}
		if err != nil {
			return false, err
		}
		_, err = s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
			_, err := tx.ExecContext(ctx, "UPDATE phase_attempts SET commit_sha=? WHERE id=?", result.SHA, a.id)
			return events.Draft{RunID: run.ID, Type: "implement.committed", Payload: map[string]any{"attempt": a.number, "commit_sha": result.SHA}}, err
		})
		return false, err
	}
	v, done, err := s.systemStep(ctx, repo, run, gitRun, systemStepRequest{Step: "checks", Commands: repo.Checks, TargetSHA: commit, OwnerID: a.id, Retry: s.recheckRequested(run, a.id)})
	if err != nil || !done {
		return false, err
	}
	if v.Status != "succeeded" {
		return false, s.repairChecks(ctx, repo, run, gitRun, a, v)
	}
	return true, g.PushImplement(ctx, gitRun, commit)
}

func checksFailure(v *workflow.SystemStepSnapshot) error {
	return &fault.Error{Code: "checks.failed", Message: "Local checks failed: " + v.Error + "\n" + v.Output}
}

func (s *Scheduler) checkResult(run workflow.Run, id string) *workflow.SystemStepSnapshot {
	for i := len(run.SystemSteps) - 1; i >= 0; i-- {
		if run.SystemSteps[i].ID == id {
			v := run.SystemSteps[i]
			return &v
		}
	}
	return nil
}

func (s *Scheduler) repairChecks(ctx context.Context, repo config.Repository, run workflow.Run, gitRun managedgit.Run, a attempt, v *workflow.SystemStepSnapshot) error {
	var spent int
	if err := s.db.QueryRowContext(ctx, "SELECT count(DISTINCT check_retry_of) FROM phase_attempts WHERE run_id=? AND phase=? AND round=? AND check_retry_of!=''", run.ID, run.Phase, phaseRound(run, run.Phase)).Scan(&spent); err != nil {
		return err
	}
	allowance := repo.CheckRetries
	for _, retry := range run.Retries {
		if !retry.Pending && retry.Error == "" && retry.NextPhase == run.Phase && retry.Round == run.ReviewRound {
			allowance += retry.GrantedCheck
		}
	}
	if spent >= allowance {
		return checksFailure(v)
	}
	run.SystemSteps = append(run.SystemSteps, *v)
	issue, _, err := s.context(ctx, run)
	if err != nil {
		return err
	}
	if run.Phase == workflow.Implement {
		return s.startAttempt(ctx, repo, run, issue, gitRun, a.number+1, v.ID)
	}
	return s.startFix(ctx, repo, run, gitRun, a.number+1, v.ID)
}

func latestChecks(run workflow.Run) *workflow.SystemStepSnapshot {
	for i := len(run.SystemSteps) - 1; i >= 0; i-- {
		v := run.SystemSteps[i]
		if v.Step == "checks" && v.Phase == run.Phase && v.Round == phaseRound(run, run.Phase) {
			return &v
		}
	}
	return nil
}

func (s *Scheduler) recheckRequested(run workflow.Run, owner string) bool {
	latest := latestChecks(run)
	if latest == nil || latest.OwnerID != owner {
		return false
	}
	for _, retry := range run.Retries {
		if !retry.Pending && retry.Error == "" && retry.CheckFrom == latest.ID {
			return true
		}
	}
	return false
}

func (s *Scheduler) retryChecks(ctx context.Context, repo config.Repository, run workflow.Run, gitRun managedgit.Run, pr *github.PullRequest, v workflow.RetrySnapshot) error {
	last := latestChecks(run)
	if last == nil || last.Status != "failed" {
		return s.rejectRetry(ctx, run, v, &fault.Error{Code: "retry.phase_ambiguous", Message: "Cannot identify failed local checks"})
	}
	target := ""
	if pr != nil {
		target = pr.Head.SHA
		if !reviewHeadMatches(pr, repo, workflow.Run{RunMetadata: workflow.RunMetadata{PRNumber: pr.Number}}, gitRun, target) {
			return s.rejectRetry(ctx, run, v, &fault.Error{Code: "retry.pr_ambiguous", Message: "PR ownership differs from preserved work"})
		}
	}
	g, ok := s.deps.Git.(RetryGit)
	if !ok {
		return s.rejectRetry(ctx, run, v, &fault.Error{Code: "git.retry_unsupported", Message: "Cannot inspect preserved work"})
	}
	if _, err := g.InspectRetry(ctx, gitRun, target, true); err != nil {
		return s.rejectRetry(ctx, run, v, err)
	}
	v.Pending = false
	v.NextState = workflow.Active
	v.NextPhase = run.Phase
	v.Round = run.ReviewRound
	v.GrantedCheck = 1
	v.CheckFrom = last.ID
	v.RecheckOwnerID = last.OwnerID
	if strings.HasPrefix(last.OwnerID, "handback:") {
		v.NextState = workflow.Manual
		v.ResumeHandback = &workflow.HandbackSnapshot{Pending: true, RequestedAt: s.deps.Now().UTC().Format(time.RFC3339Nano)}
	}
	selected, err := s.workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.Retry, NextState: v.NextState, NextPhase: v.NextPhase, Metadata: workflow.MetadataPatch{Retry: &v}, AgentPorts: &s.cfg.AgentPorts})
	if err != nil {
		return s.rejectRetry(ctx, run, v, err)
	}
	return s.retryLabels(ctx, repo, selected)
}

func checkRetryIdentity(run workflow.Run, phase workflow.Phase, previous attempt, number int, override []string) string {
	if len(override) > 0 {
		return override[0]
	}
	if floor := attemptWindowStart(run, phase); number == floor && number > previous.number {
		return ""
	}
	return previous.checkRetryOf
}
