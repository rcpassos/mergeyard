package scheduler

import (
	"context"
	"database/sql"
	"errors"

	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// Takeover prepares manual control and returns the prepared run snapshot. The
// CLI owns the interactive terminal and rebuilds the resume invocation locally
// from the stored role settings; the runtime never returns a command to run.
// The shared run gate excludes in-flight publication, review and retry work.
func (s *Scheduler) Takeover(ctx context.Context, id string) (workflow.Run, error) {
	var prepared workflow.Run
	err := s.workflow.WithRunOperation(ctx, id, func(ctx context.Context, run workflow.Run) error {
		if run.State.Terminal() || run.Merge != nil {
			return &fault.Error{Code: "takeover.unavailable", Message: "Takeover requires a nonterminal run without merged-PR cleanup"}
		}
		var stopping bool
		if err := s.db.QueryRowContext(ctx, "SELECT stop_requested FROM runs WHERE id=?", id).Scan(&stopping); err != nil {
			return err
		}
		if stopping || run.PendingRetry() != nil || run.PendingHandback() != nil {
			return &fault.Error{Code: "takeover.operation_pending", Message: "Finish the pending Stop or Retry before requesting takeover"}
		}
		// Validate the prerequisites before recording takeover intent, so a
		// missing worktree or session leaves no requested marker behind.
		if _, err := s.interactiveCommand(ctx, run); err != nil {
			return err
		}
		if run.State == workflow.Manual && run.TakeoverStatus == workflow.TakeoverManual {
			_, gitRun, err := s.context(ctx, run)
			if err == nil {
				err = s.deps.Git.Inspect(ctx, gitRun)
			}
			if err != nil {
				repo, _ := s.runRepository(run)
				return errors.Join(err, s.recordAttention(ctx, repo, run, err))
			}
		} else {
			if run.TakeoverStatus != workflow.TakeoverRequested {
				if _, err := s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
					_, err := tx.ExecContext(ctx, "UPDATE runs SET takeover_status='requested' WHERE id=?", id)
					return events.Draft{RunID: id, Type: "run.takeover_requested", Payload: map[string]string{"status": "requested"}}, err
				}); err != nil {
					return err
				}
			}
			if err := s.prepareTakeover(ctx, run); err != nil {
				return err
			}
		}
		var err error
		prepared, err = s.workflow.Get(ctx, id)
		return err
	})
	if err != nil {
		return workflow.Run{}, err
	}
	return prepared, nil
}

func (s *Scheduler) interactiveCommand(ctx context.Context, run workflow.Run) (harness.InteractiveCommand, error) {
	if _, ok := s.runRepository(run); !ok {
		return harness.InteractiveCommand{}, &fault.Error{Code: "config.repository_missing", Message: "Restore the run's repository configuration before takeover"}
	}
	_, gitRun, err := s.context(ctx, run)
	if err != nil || gitRun.ID == "" || gitRun.Path == "" {
		return harness.InteractiveCommand{}, &fault.Error{Code: "takeover.worktree_missing", Message: "Wait for Mergeyard to prepare this run's worktree before takeover", Err: err}
	}
	agent, session := run.TakeoverResume()
	return harness.InteractiveResume(s.harnesses, agent, session, gitRun.Path)
}

// ManualCommand only exposes commands after safe preparation has completed.
func (s *Scheduler) ManualCommand(ctx context.Context, id string) (harness.InteractiveCommand, error) {
	run, err := s.workflow.Get(ctx, id)
	if err != nil {
		return harness.InteractiveCommand{}, err
	}
	if run.State != workflow.Manual || run.TakeoverStatus != workflow.TakeoverManual || run.PendingHandback() != nil {
		return harness.InteractiveCommand{}, &fault.Error{Code: "takeover.unavailable", Message: "Prepare takeover before resuming the implementer"}
	}
	_, gitRun, err := s.context(ctx, run)
	if err != nil {
		return harness.InteractiveCommand{}, err
	}
	if err := s.deps.Git.Inspect(ctx, gitRun); err != nil {
		return harness.InteractiveCommand{}, err
	}
	return s.interactiveCommand(ctx, run)
}

func (s *Scheduler) prepareTakeover(ctx context.Context, run workflow.Run) error {
	repo, ok := s.runRepository(run)
	if !ok {
		return &fault.Error{Code: "config.repository_missing", Message: "Restore the run's repository configuration before takeover"}
	}
	var stopping bool
	if err := s.db.QueryRowContext(ctx, "SELECT stop_requested FROM runs WHERE id=?", run.ID).Scan(&stopping); err != nil {
		return err
	}
	if stopping {
		return s.stopRun(ctx, run)
	}
	fail := func(cause error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.Join(cause, s.recordAttention(ctx, repo, run, cause))
	}
	if _, err := s.interactiveCommand(ctx, run); err != nil {
		return fail(err)
	}
	var untracked int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM phase_attempts WHERE run_id=? AND status='running' AND (COALESCE(process_session,'')='' OR COALESCE(input_path,'')='')`, run.ID).Scan(&untracked); err != nil {
		return fail(err)
	}
	if untracked > 0 {
		return fail(&fault.Error{Code: "takeover.process_ambiguous", Message: "An automated attempt has no recorded process identity; inspect preserved work before takeover"})
	}
	if err := s.stopOwnedPhases(ctx, run); err != nil {
		return fail(err)
	}
	refs, err := s.runSessions(ctx, run.ID)
	if err != nil {
		return fail(err)
	}
	for _, ref := range refs {
		status, err := s.deps.Runner.SessionStatus(ctx, ref)
		if err != nil {
			return fail(err)
		}
		if status.State != runner.SessionExited {
			return fail(&fault.Error{Code: "takeover.process_ambiguous", Message: "Automated process exit cannot be verified; inspect preserved work before requesting takeover again"})
		}
	}
	_, gitRun, err := s.context(ctx, run)
	if err != nil {
		return fail(err)
	}
	// Restore even if a prior interruption moved the run into attention.
	a, err := s.lastReview(ctx, run)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fail(err)
	}
	if err == nil && !a.restored {
		status, err := s.deps.Runner.SessionStatus(ctx, a.Ref)
		if err != nil {
			return fail(err)
		}
		if status.State != runner.SessionExited {
			return fail(&fault.Error{Code: "takeover.process_ambiguous", Message: "Reviewer exit cannot be verified; inspect the preserved process and worktree"})
		}
		if err := s.restoreReview(ctx, gitRun, &a); err != nil {
			return fail(err)
		}
	}
	if err := s.deps.Git.Inspect(ctx, gitRun); err != nil {
		return fail(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE phase_attempts SET status='stopped',ended_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE run_id=? AND status='running'`, run.ID); err != nil {
		return err
	}
	_, err = s.workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.TakeOver})
	return err
}
