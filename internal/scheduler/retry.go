package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type RetryGit interface {
	InspectRetry(context.Context, managedgit.Run, string, bool) (string, error)
}

// Retry shares the operation gate with stop, reconciliation, and every phase.
// It selects work durably; only later scheduler ticks can launch that work.
func (s *Scheduler) Retry(ctx context.Context, id string) (workflow.Run, error) {
	var result workflow.Run
	err := s.workflow.WithRunOperation(ctx, id, func(ctx context.Context, run workflow.Run) error {
		if run.TakeoverStatus == workflow.TakeoverRequested {
			return &fault.Error{Code: "takeover.operation_pending", Message: "Finish takeover or Stop before requesting Retry"}
		}
		if !run.RetryAllowed() {
			if len(run.Retries) > 0 && run.State != workflow.Stopped && run.State != workflow.Manual {
				result = run
				return nil
			}
			return &fault.Error{Code: "retry.unavailable", Message: "Retry requires a failed or needs-attention run"}
		}
		if run.State == workflow.WaitingForHarness {
			for _, probe := range run.CreditProbes {
				if probe.Status == "reserved" || probe.Status == "running" {
					result = run
					return nil
				}
			}
		}
		repo, ok := s.repository(run.Repository)
		if !ok {
			return &fault.Error{Code: "config.repository_missing", Message: "Run repository is no longer configured"}
		}
		if run.PendingRetry() == nil {
			v := workflow.RetrySnapshot{Pending: true, RequestedAt: s.deps.Now().UTC().Format(time.RFC3339Nano), Cause: run.LastErrorCode}
			data, err := json.Marshal(v)
			if err != nil {
				return err
			}
			_, err = s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
				_, err := tx.ExecContext(ctx, "INSERT INTO run_retries(run_id,pending,snapshot_json) VALUES(?,1,?)", run.ID, string(data))
				return events.Draft{RunID: run.ID, Type: "run.retry_requested", Payload: v}, err
			})
			if err != nil {
				return err
			}
			run, err = s.workflow.Get(ctx, id)
			if err != nil {
				return err
			}
		}
		err := s.resumeRetry(ctx, repo, run)
		var getErr error
		result, getErr = s.workflow.Get(ctx, id)
		if getErr != nil {
			return getErr
		}
		return err
	})
	return result, err
}

func (s *Scheduler) rejectRetry(ctx context.Context, run workflow.Run, v workflow.RetrySnapshot, cause error) error {
	if err := s.saveRetryRejection(ctx, run, v, cause); err != nil {
		return err
	}
	return cause
}

func (s *Scheduler) saveRetryRejection(ctx context.Context, run workflow.Run, v workflow.RetrySnapshot, cause error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	} // Pending intent survives interruption.
	v.Pending = false
	v.Error = safeRetryError(cause)
	slog.ErrorContext(ctx, "Retry reconciliation rejected", "run_id", run.ID, "error", cause)
	_, err := s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		return events.Draft{RunID: run.ID, Type: "run.retry_rejected", Payload: v}, v.Save(ctx, tx, run.ID)
	})
	return err
}

func (s *Scheduler) resumeRetry(ctx context.Context, repo config.Repository, run workflow.Run) error {
	intent := run.PendingRetry()
	if intent == nil {
		return nil
	}
	v := *intent
	fail := func(code, message string) error {
		return s.rejectRetry(ctx, run, v, &fault.Error{Code: code, Message: message})
	}
	var stopping bool
	if err := s.db.QueryRowContext(ctx, "SELECT stop_requested FROM runs WHERE id=?", run.ID).Scan(&stopping); err != nil {
		return err
	}
	if run.Merge != nil {
		return s.finishMerge(ctx, run)
	}
	// Observe a merge before any Git check or attempt selection, even for FAILED.
	_, gitRun, contextErr := s.context(ctx, run)
	var pr *github.PullRequest
	var err error
	if run.PRNumber > 0 {
		pr, err = s.deps.GitHub.GetPullRequest(ctx, repo.Repo, run.PRNumber)
	} else if gitRun.ID != "" {
		pr, err = s.deps.GitHub.FindOpenPullRequest(ctx, repo.Repo, gitRun.Branch)
	}
	if err != nil && !stopping {
		return err
	} // Unavailable service keeps intent pending.
	if pr != nil && pr.Merged {
		return s.merged(ctx, repo, run, pr)
	}
	if stopping {
		// Stop supersedes pending Retry. A failed PR lookup cannot delay
		// interruption, and a later tick must not resume this retry intent.
		if run.State.Terminal() {
			// Terminal runs rely on pending Retry to remain reconcilable.
			// Retain that intent until interruption is verified complete.
			if err := s.stopOwnedPhases(ctx, run); err != nil {
				return err
			}
		}
		if err := s.saveRetryRejection(ctx, run, v, &fault.Error{Code: "retry.stop_pending", Message: "Stop superseded the pending Retry"}); err != nil {
			return err
		}
		if run.State.Terminal() {
			return nil
		}
		return s.stopRun(ctx, run)
	}
	if run.PRNumber > 0 && pr == nil {
		return fail("retry.pr_ambiguous", "Saved PR could not be observed; inspect GitHub before retrying")
	}
	if pr != nil && pr.State != github.Open {
		return fail("retry.pr_closed", "PR closed without merging; reopen it or inspect preserved work")
	}
	issue, err := s.deps.GitHub.GetIssue(ctx, repo.Repo, run.IssueNumber)
	if err != nil {
		return err
	}
	if issue.State != github.Open {
		return fail("github.issue_closed", "Issue is closed; reopen it before retrying")
	}
	if errors.Is(contextErr, sql.ErrNoRows) && gitRun.ID == "" {
		if err := s.saveIssue(ctx, run.ID, issue); err != nil {
			return err
		}
		contextErr = nil
	}
	if contextErr != nil {
		return s.rejectRetry(ctx, run, v, &fault.Error{Code: "reconcile.context_missing", Message: "Run context cannot be reconciled; inspect saved artifacts", Err: contextErr})
	}
	// Check every recorded process, including failed launch acknowledgements.
	rows, err := s.db.QueryContext(ctx, "SELECT process_session,COALESCE(input_path,'') FROM phase_attempts WHERE run_id=? AND COALESCE(process_session,'')!=''", run.ID)
	if err != nil {
		return err
	}
	var refs []runner.SessionRef
	for rows.Next() {
		var name, input string
		if err := rows.Scan(&name, &input); err != nil {
			rows.Close()
			return err
		}
		refs = append(refs, runner.SessionRef{Name: name, PhaseDir: filepath.Dir(input)})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	missingProcesses := map[string]bool{}
	for _, ref := range refs {
		status, err := s.deps.Runner.SessionStatus(ctx, ref)
		if err != nil {
			return s.rejectRetry(ctx, run, v, err)
		}
		if status.State == runner.SessionRunning {
			return fail("retry.process_running", "An owned process is still running; wait or stop it before retrying")
		}
		missingProcesses[ref.Name] = status.State == runner.SessionMissing
	}
	if run.Review != nil && !run.Review.Restored {
		// Explicit retry cannot distinguish reviewer changes from later user edits.
		return fail("retry.review_unrestored", "Review restoration is unfinished; inspect preserved work before retrying")
	}
	if v.Cause == waitsExhausted {
		wait := run.HarnessWait
		if wait == nil || wait.AttemptID == "" || wait.Phase != run.Phase || wait.Round != phaseRound(run, run.Phase) {
			return fail("retry.phase_ambiguous", "Cannot identify the interrupted execution")
		}
		if pr != nil {
			target := ""
			if run.Phase == workflow.Review && run.Review != nil {
				target = run.Review.TargetSHA
			}
			if run.Phase == workflow.Fix && run.Fix != nil {
				target = run.Fix.TargetSHA
			}
			if !reviewHeadMatches(pr, repo, run, gitRun, target) {
				return fail("retry.head_ambiguous", "PR changed during usage-limit waiting")
			}
		}
		g, ok := s.deps.Git.(RetryGit)
		if !ok {
			return fail("git.retry_unsupported", "Git adapter cannot inspect preserved work")
		}
		target := ""
		if pr != nil {
			target = pr.Head.SHA
		}
		if _, err := g.InspectRetry(ctx, gitRun, target, run.Phase != workflow.Review); err != nil {
			return s.rejectRetry(ctx, run, v, err)
		}
		v.Pending = false
		v.NextState, v.NextPhase, v.Round = workflow.WaitingForHarness, run.Phase, run.ReviewRound
		v.GrantedWait, v.WaitSequence = wait.Allowance+1, wait.Sequence
		patch := workflow.MetadataPatch{Retry: &v}
		if wait.Reason == "credits_exhausted" {
			v.NextState = workflow.Active
			if err := s.selectCreditProbe(ctx, run, repo, v, &patch); err != nil {
				return s.rejectRetry(ctx, run, v, err)
			}
		}
		selected, err := s.workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.Retry, NextState: v.NextState, NextPhase: v.NextPhase, Metadata: patch})
		if err != nil {
			v.Pending = true
			return s.rejectRetry(ctx, run, v, err)
		}
		return s.retryLabels(ctx, repo, selected)
	}
	v.Round = run.ReviewRound
	v.NextState, v.NextPhase = workflow.Active, run.Phase
	patch := workflow.MetadataPatch{Retry: &v}
	var observedCI *ci.Snapshot
	if pr != nil {
		patch.PRNumber = &pr.Number
		v.TargetSHA = pr.Head.SHA
		if !reviewHeadMatches(pr, repo, workflow.Run{RunMetadata: workflow.RunMetadata{PRNumber: pr.Number}}, gitRun, pr.Head.SHA) {
			return fail("retry.pr_ambiguous", "PR ownership differs from the run branch or repository")
		}
		// Invalidation must survive a rejected retry (for example dirty local work).
		if run.ApprovedSHA != "" && run.ApprovedSHA != pr.Head.SHA {
			if _, err := s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
				_, err := tx.ExecContext(ctx, "UPDATE runs SET approved_sha=NULL WHERE id=?", run.ID)
				return events.Draft{RunID: run.ID, Type: "review.approval_invalidated", Payload: map[string]string{"head": pr.Head.SHA}}, err
			}); err != nil {
				return err
			}
			run.ApprovedSHA = ""
		}
		reviewed := run.Review != nil && run.Review.Accepted && run.Review.Report != nil && run.Review.Report.Status == "approved" && run.Review.TargetSHA == pr.Head.SHA
		approved := reviewed && run.ApprovedSHA == pr.Head.SHA
		if reviewed && run.Phase == workflow.Fix && run.CI != nil && run.CI.SHA == pr.Head.SHA && run.CI.RepairCause != "" {
			// A failed repair does not authorize acting on saved check failures.
			// Reconcile current CI before deciding whether any fixer is needed.
			target := *pr
			wait := s.observeRetryCI(ctx, repo, run, target)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fresh, err := s.deps.GitHub.GetPullRequest(ctx, repo.Repo, pr.Number)
			if err != nil {
				return err
			}
			if fresh != nil && fresh.Merged {
				return s.merged(ctx, repo, run, fresh)
			}
			if !reviewHeadMatches(fresh, repo, run, gitRun, target.Head.SHA) || !sameCITarget(target, *fresh) {
				return fail("retry.head_ambiguous", "PR changed during CI reconciliation; inspect before retrying")
			}
			_, attention := wait.Evidence.Gate()
			if wait.QueryError == "" && (attention == "ci.check_failed" || attention == "ci.check_timed_out") {
				wait.RepairCause = attention
				patch.CI = &wait
				v.Deadline = wait.Deadline.Format(time.RFC3339Nano)
			} else {
				// The independent review still covers this unchanged published head.
				// Clean-worktree inspection below is required before restoring approval.
				approved = true
				sha := target.Head.SHA
				patch.ApprovedSHA = &sha
				observedCI = &wait
			}
		}
		v.NextState, v.NextPhase = retryPRWork(run, pr.Head.SHA, approved)
		if v.Round < 1 {
			v.Round = 1
			patch.ReviewRound = &v.Round
		}
	} else if gitRun.ID == "" {
		v.NextState, v.NextPhase = workflow.Claiming, ""
	} else {
		v.NextPhase = workflow.Implement
	}
	if gitRun.ID != "" {
		g, ok := s.deps.Git.(RetryGit)
		if !ok {
			return fail("git.retry_unsupported", "Git adapter cannot safely inspect retry work")
		}
		allowEdits := v.NextState == workflow.Active && (v.NextPhase == workflow.Implement || v.NextPhase == workflow.Fix)
		if _, err := g.InspectRetry(ctx, gitRun, v.TargetSHA, allowEdits); err != nil {
			return s.rejectRetry(ctx, run, v, err)
		}
	}
	if v.Cause == "review.max_rounds_exceeded" && v.NextState == workflow.Active {
		v.GrantedRound = run.ReviewRound + 1
	}
	if v.NextState == workflow.WaitingForCI {
		// Observe CI before selecting the wait. The regular gate repeats observation
		// before readiness or repair and handles every native conclusion.
		var wait ci.Snapshot
		if observedCI != nil {
			wait = *observedCI
		} else {
			wait = s.observeRetryCI(ctx, repo, run, *pr)
		}
		patch.CI = &wait
		v.Deadline = wait.Deadline.Format(time.RFC3339Nano)
		// Exhausted CI repair needs a subsequent independently reviewed round.
		if v.Cause == "review.max_rounds_exceeded" {
			v.GrantedRound = run.ReviewRound + 1
		}
	} else if v.NextState == workflow.Active {
		var a attempt
		switch v.NextPhase {
		case workflow.Implement:
			a, err = s.lastAttempt(ctx, run.ID)
		case workflow.Review:
			var r reviewAttempt
			r, err = s.lastReview(ctx, run)
			a = r.attempt
		case workflow.Fix:
			var f fixAttempt
			f, err = s.lastFix(ctx, run)
			a = f.attempt
		default:
			return fail("retry.phase_ambiguous", "Cannot identify a safe next phase")
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && (a.status == "failed" || a.status == "stopped" || (a.status == "running" && missingProcesses[a.ref.Name]) || (a.status == "succeeded" && v.NextPhase == workflow.Review)) {
			if missingConversation(attemptCause(a)) {
				var recovered int
				if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM session_recoveries WHERE run_id=? AND phase=? AND round=?", run.ID, v.NextPhase, phaseRound(run, v.NextPhase)).Scan(&recovered); err != nil {
					return err
				}
				if recovered > 0 || !a.resumed {
					return fail("harness.session_resume_failed", "Missing-session recovery was already used for this phase and round; inspect the role conversation")
				}
			}
			v.AttemptFrom = a.number + 1
		}
	}
	if err := s.selectCreditProbe(ctx, run, repo, v, &patch); err != nil {
		return s.rejectRetry(ctx, run, v, err)
	}
	v.Pending = false
	selected, err := s.workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.Retry, NextState: v.NextState, NextPhase: v.NextPhase, Metadata: patch})
	if err != nil {
		v.Pending = true
		return s.rejectRetry(ctx, run, v, err)
	}
	if selected.State == workflow.WaitingForCI {
		if err := s.waitCI(ctx, repo, selected); err != nil {
			return err
		}
		return s.retryProgressLabels(ctx, repo, selected)
	}
	return s.retryLabels(ctx, repo, selected)
}

// Label failures remain retryable bookkeeping, after CI observation so they
// cannot delay expiration or restore progress labels on a new attention state.
func (s *Scheduler) retryProgressLabels(ctx context.Context, repo config.Repository, run workflow.Run) error {
	if len(run.Retries) == 0 && len(run.Handbacks) == 0 {
		return nil
	}
	current, err := s.workflow.Get(ctx, run.ID)
	if err != nil {
		return err
	}
	if current.Merge != nil || (current.State != workflow.Active && current.State != workflow.WaitingForCI && current.State != workflow.ReadyToMerge) {
		return nil
	}
	return s.retryLabels(ctx, repo, current)
}

func (s *Scheduler) observeRetryCI(ctx context.Context, repo config.Repository, run workflow.Run, pr github.PullRequest) ci.Snapshot {
	now := s.deps.Now().UTC()
	wait := ci.Snapshot{SHA: pr.Head.SHA}
	if run.CI != nil {
		wait = *run.CI
	}
	wait.SHA, wait.CurrentHead = pr.Head.SHA, pr.Head.SHA
	wait.StartedAt, wait.Deadline = now, now.Add(s.cfg.CITimeout)
	wait.ReadyStarted = false
	wait.Warning, wait.RepairCause, wait.QueryError = "", "", ""
	api, ok := s.deps.GitHub.(CIGitHub)
	if !ok {
		wait.Evidence = ci.Evidence{}
		wait.QueryError = "GitHub adapter cannot establish check requirements"
		return wait
	}
	var err error
	wait.Evidence, err = api.PullRequestEvidence(ctx, repo.Repo, pr)
	wait.Evidence.Gate()
	if err != nil {
		slog.ErrorContext(ctx, "Retry CI observation failed", "run_id", run.ID, "error", err)
		wait.QueryError = "CI evidence is unavailable; reconciliation will retry"
	}
	if wait.Evidence.SHA != pr.Head.SHA || (wait.Evidence.MergeSHA != "" && wait.Evidence.MergeSHA != pr.MergeCommitSHA) || pr.Base.Ref == "" {
		wait.QueryError = "CI evidence does not establish the current PR target"
	}
	return wait
}

func safeRetryError(cause error) string {
	var coded *fault.Error
	if !errors.As(cause, &coded) {
		return "internal.run_operation: Could not reconcile Retry; check the application log"
	}
	messages := map[string]string{
		"retry.unavailable":             "Retry requires a failed or needs-attention run",
		"retry.stop_pending":            "Finish the pending Stop before retrying",
		"retry.process_running":         "An owned process is still running; wait or stop it before retrying",
		"retry.pr_closed":               "PR closed without merging; reopen it or inspect preserved work",
		"retry.pr_ambiguous":            "PR ownership or observation is ambiguous; inspect GitHub before retrying",
		"retry.head_ambiguous":          "PR and Git observations disagree; inspect before retrying",
		"retry.head_diverged":           "Local head differs from the published head; align preserved work before retrying",
		"retry.worktree_dirty":          "Preserved edits must be committed and published or moved aside before review or CI waiting",
		"retry.review_unrestored":       "Review restoration is unfinished; inspect preserved work before retrying",
		"retry.phase_ambiguous":         "Cannot identify a safe next phase; inspect preserved artifacts",
		"github.issue_closed":           "Issue is closed; reopen it before retrying",
		"harness.session_resume_failed": "Missing-session recovery was already used for this phase and round; inspect the role conversation",
		"internal.run_conflict":         "Another active run owns this issue; inspect it before retrying",
	}
	if message, ok := messages[coded.Code]; ok {
		return coded.Code + ": " + message
	}
	return "internal.run_operation: Could not reconcile Retry; check the application log"
}

func (s *Scheduler) retryLabels(ctx context.Context, repo config.Repository, run workflow.Run) error {
	if err := s.deps.GitHub.AddLabel(ctx, repo.Repo, run.IssueNumber, repo.Labels.Running); err != nil {
		return err
	}
	if err := s.deps.GitHub.RemoveLabel(ctx, repo.Repo, run.IssueNumber, repo.Labels.Ready); err != nil {
		return err
	}
	return s.deps.GitHub.RemoveLabel(ctx, repo.Repo, run.IssueNumber, repo.Labels.NeedsAttention)
}

func attemptCause(a attempt) error {
	code, message, _ := strings.Cut(a.failure, ": ")
	return &fault.Error{Code: code, Message: message}
}

func explicitAttempt(run workflow.Run, phase workflow.Phase, number int) int {
	if floor := attemptWindowStart(run, phase); floor > number {
		return floor
	}
	return 0
}

// Explicit Retry and successful handback both open a configured attempt window.
// Use the same absolute floor for launching work and accounting for failures.
func attemptWindowStart(run workflow.Run, phase workflow.Phase) int {
	floor := 1
	for _, v := range run.Handbacks {
		if !v.Pending && v.Error == "" && v.NextPhase == phase && v.Round == run.ReviewRound && v.AttemptFrom > floor {
			floor = v.AttemptFrom
		}
	}
	for _, v := range run.Retries {
		if !v.Pending && v.Error == "" && v.NextPhase == phase && v.Round == run.ReviewRound && v.AttemptFrom > floor {
			floor = v.AttemptFrom
		}
	}
	return floor
}

func (s *Scheduler) roundLimit(run workflow.Run) int {
	max := s.cfg.MaxRounds
	for _, v := range run.Handbacks {
		if !v.Pending && v.Error == "" && v.GrantedRound > max {
			max = v.GrantedRound
		}
	}
	for _, v := range run.Retries {
		if !v.Pending && v.Error == "" && v.GrantedRound > max {
			max = v.GrantedRound
		}
	}
	return max
}

// retryPRWork selects work from reconciled PR evidence. Snapshot eligibility uses
// the same selection with the latest locally observed head; Retry refreshes it.
func retryPRWork(run workflow.Run, head string, approved bool) (workflow.State, workflow.Phase) {
	if approved {
		return workflow.WaitingForCI, workflow.Review
	}
	if run.Phase == workflow.Fix && run.Fix != nil && run.Fix.Status == "succeeded" && run.Fix.CommitSHA == head {
		return workflow.Active, workflow.Fix
	}
	if run.Phase == workflow.Fix && run.Review != nil && run.Review.TargetSHA == head {
		return workflow.Active, workflow.Fix
	}
	if run.Review != nil && run.Review.Accepted && run.Review.TargetSHA == head && run.Review.Report != nil && run.Review.Report.Status == "changes_required" {
		return workflow.Active, workflow.Fix
	}
	return workflow.Active, workflow.Review
}
