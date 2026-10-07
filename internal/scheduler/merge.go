package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/maintenance"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// MergeGit separates destructive maintenance boundaries so each can be journaled.
type MergeGit interface {
	CleanupWorktree(context.Context, managedgit.Run) error
	CleanupBranch(context.Context, managedgit.Run) error
}
type MergeGitHub interface {
	CloseIssue(context.Context, string, int) error
}

func (s *Scheduler) saveMerge(ctx context.Context, run workflow.Run, v maintenance.Snapshot, event string) error {
	_, err := s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		if _, err := tx.ExecContext(ctx, "UPDATE runs SET stop_requested=1 WHERE id=?", run.ID); err != nil {
			return events.Draft{}, err
		}
		return events.Draft{RunID: run.ID, Type: event, Payload: v}, maintenance.Save(ctx, tx, run.ID, v)
	})
	return err
}
func (s *Scheduler) merged(ctx context.Context, repo config.Repository, run workflow.Run, pr *github.PullRequest) error {
	if run.State == workflow.Stopped || run.State == workflow.Completed {
		return nil
	}
	if run.Merge == nil {
		v := maintenance.Snapshot{ObservedAt: s.deps.Now().UTC().Format(time.RFC3339Nano), Early: run.State != workflow.ReadyToMerge, PublishedSHA: pr.Head.SHA,
			ReadyLabel: repo.Labels.Ready, RunningLabel: repo.Labels.Running, AttentionLabel: repo.Labels.NeedsAttention}
		if err := s.saveMerge(ctx, run, v, "pr.merge_observed"); err != nil {
			return err
		}
		run.Merge = &v
	}
	return s.finishMerge(ctx, run)
}
func (s *Scheduler) finishMerge(ctx context.Context, run workflow.Run) error {
	v := *run.Merge
	if !v.ProcessExited {
		if err := s.stopOwnedPhases(ctx, run); err != nil {
			return s.mergeFailure(ctx, run, v, err)
		}
		// Keep review restoration in maintenance: unsafe reviewer mutations must not
		// hold coding capacity after the owned process is verified stopped.
		if _, err := s.db.ExecContext(ctx, `UPDATE phase_attempts SET status='stopped',ended_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE run_id=? AND status='running'`, run.ID); err != nil {
			return err
		}
		v.ProcessExited = true
		if err := s.saveMerge(ctx, run, v, "merge.cleanup_updated"); err != nil {
			return err
		}
	}
	if run.State != workflow.Completed {
		request := workflow.Request{Trigger: workflow.PRMerged}
		if run.State == workflow.Failed {
			request.Trigger = workflow.Retry
			request.NextState = workflow.Completed
		}
		if v := run.PendingRetry(); v != nil {
			v.Pending = false
			v.NextState = workflow.Completed
			request.Metadata.Retry = v
		}
		if _, err := s.workflow.Transition(ctx, run.ID, request); err != nil {
			return err
		}
		run.State = workflow.Completed
	}
	run.Merge = &v
	return s.cleanupMerge(ctx, run)
}
func (s *Scheduler) mergeFailure(ctx context.Context, run workflow.Run, v maintenance.Snapshot, cause error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if v.Error == cause.Error() {
		return nil
	}
	v.Error = cause.Error()
	// External maintenance failures are recorded, not allowed to block dispatch.
	return s.saveMerge(ctx, run, v, "merge.cleanup_updated")
}
func (s *Scheduler) cleanupMerge(ctx context.Context, run workflow.Run) error {
	v := *run.Merge
	if !v.Pending() {
		return nil
	}
	// Bound optional maintenance so unavailable adapters cannot monopolize a tick.
	cleanupContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	step := func(done *bool, action func() error) error {
		if *done {
			return nil
		}
		if err := action(); err != nil {
			return err
		}
		*done, v.Error = true, ""
		return s.saveMerge(ctx, run, v, "merge.cleanup_updated")
	}
	// Attempt independent GitHub maintenance even if another action is unavailable.
	var failures []error
	for _, label := range []struct {
		done *bool
		name string
	}{{&v.ReadyRemoved, v.ReadyLabel}, {&v.RunningRemoved, v.RunningLabel}, {&v.AttentionRemoved, v.AttentionLabel}} {
		if err := step(label.done, func() error {
			return s.deps.GitHub.RemoveLabel(cleanupContext, run.Repository, run.IssueNumber, label.name)
		}); err != nil {
			failures = append(failures, err)
		}
	}
	if err := step(&v.IssueClosed, func() error {
		issue, err := s.deps.GitHub.GetIssue(cleanupContext, run.Repository, run.IssueNumber)
		if err != nil || issue.State == github.Closed {
			return err
		}
		api, ok := s.deps.GitHub.(MergeGitHub)
		if !ok {
			return &fault.Error{Code: "github.cleanup_unsupported", Message: "GitHub adapter cannot close issues"}
		}
		if err := api.CloseIssue(cleanupContext, run.Repository, run.IssueNumber); err != nil {
			return err
		}
		issue, err = s.deps.GitHub.GetIssue(cleanupContext, run.Repository, run.IssueNumber)
		if err != nil {
			return err
		}
		if issue.State != github.Closed {
			return &fault.Error{Code: "github.close_pending", Message: "Issue closure has not been observed"}
		}
		return nil
	}); err != nil {
		failures = append(failures, err)
	}
	// Share ownership with CLI resumes and retain it across every Git mutation.
	// A separate status check would permit a launch between checking and removal.
	lock, lockErr := runner.AcquireInteractive(s.workspace.Root, run.ID)
	if lockErr != nil {
		return s.mergeFailure(ctx, run, v, errors.Join(append(failures, lockErr)...))
	}
	defer lock.Close()
	_, gitRun, gitErr := s.context(ctx, run)
	gitRun.PublishedSHA = v.PublishedSHA
	if gitErr == nil {
		gitErr = step(&v.ReviewRestored, func() error {
			if run.Review == nil || run.Review.Restored {
				return nil
			}
			a, err := s.lastReview(ctx, run)
			if err != nil {
				return err
			}
			return s.restoreReview(cleanupContext, gitRun, &a)
		})
	}
	if gitErr == nil && !v.WorktreeRemoved {
		if !v.WorktreeStarted {
			gitErr = s.deps.Git.Inspect(cleanupContext, gitRun)
			if gitErr == nil {
				v.WorktreeStarted = true
				gitErr = s.saveMerge(ctx, run, v, "merge.cleanup_updated")
			}
		}
		if gitErr == nil {
			g, ok := s.deps.Git.(MergeGit)
			if !ok {
				gitErr = &fault.Error{Code: "git.cleanup_unsupported", Message: "Git adapter cannot clean merged runs"}
			} else {
				gitErr = step(&v.WorktreeRemoved, func() error { return g.CleanupWorktree(cleanupContext, gitRun) })
			}
		}
	}
	if gitErr == nil && v.WorktreeRemoved {
		g, ok := s.deps.Git.(MergeGit)
		if !ok {
			gitErr = &fault.Error{Code: "git.cleanup_unsupported", Message: "Git adapter cannot delete local branches"}
		} else {
			gitErr = step(&v.BranchDeleted, func() error { return g.CleanupBranch(cleanupContext, gitRun) })
		}
	}
	failures = append(failures, gitErr)
	if err := errors.Join(failures...); err != nil {
		return s.mergeFailure(ctx, run, v, err)
	}
	return nil
}
