package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/execution/durable"
	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// Stop interrupts the phase and preserves all code. Persisting intent first
// prevents a failed label write or restart from relaunching interrupted work.
func (s *Scheduler) Stop(ctx context.Context, id string) error {
	return s.workflow.WithRunOperation(ctx, id, func(ctx context.Context, run workflow.Run) error {
		if run.Merge != nil && !run.State.Terminal() {
			return s.finishMerge(ctx, run)
		}
		if run.State == workflow.Stopped {
			return nil
		}
		if run.State.Terminal() {
			return &fault.Error{Code: "run.terminal", Message: "Run is already terminal"}
		}
		if _, ok := s.runRepository(run); !ok {
			return &fault.Error{Code: "config.repository_missing", Message: "Run repository is no longer configured"}
		}
		if _, err := s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
			_, err := tx.ExecContext(ctx, "UPDATE runs SET stop_requested=1 WHERE id=?", id)
			return events.Draft{RunID: id, Type: "run.stop_requested", Payload: map[string]bool{"stop_requested": true}}, err
		}); err != nil {
			return err
		}
		return s.stopRun(ctx, run)
	})
}

func (s *Scheduler) stopRun(ctx context.Context, run workflow.Run) error {
	repo, ok := s.runRepository(run)
	if !ok {
		return &fault.Error{Code: "config.repository_missing", Message: "Run repository is no longer configured"}
	}
	if err := s.stopOwnedPhases(ctx, run); err != nil {
		return err
	}
	if run.Phase == workflow.Implement {
		a, err := s.lastAttempt(ctx, run.ID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if err := s.discoverImplementSession(ctx, run.ID, &a); err != nil {
				if !durable.InvalidDiscovery(err) {
					return err
				}
				if _, saveErr := s.db.ExecContext(ctx, "UPDATE phase_attempts SET error=? WHERE id=?", err.Error(), a.ID); saveErr != nil {
					return saveErr
				}
			}
		}
	}
	if run.Phase == workflow.Fix || run.Phase == workflow.Review {
		var a durable.Attempt
		var err error
		if run.Phase == workflow.Fix {
			var fix fixAttempt
			fix, err = s.lastFix(ctx, run)
			a = fix.Attempt
		} else {
			var review reviewAttempt
			review, err = s.lastReview(ctx, run)
			a = review.Attempt
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if err := s.discoverSession(ctx, run.ID, run.Phase, &a); err != nil {
				if !durable.InvalidDiscovery(err) {
					return err
				}
				if _, saveErr := s.db.ExecContext(ctx, "UPDATE phase_attempts SET error=? WHERE id=?", err.Error(), a.ID); saveErr != nil {
					return saveErr
				}
			}
		}
	}
	if run.Phase == workflow.Review {
		a, err := s.lastReview(ctx, run)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && !a.restored {
			_, gitRun, err := s.context(ctx, run)
			if err != nil {
				return err
			}
			if err := s.restoreReview(ctx, gitRun, &a); err != nil {
				if attentionErr := s.recordAttention(ctx, repo, run, err); attentionErr != nil {
					return attentionErr
				}
				return err
			}
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE phase_attempts SET status='stopped',ended_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE run_id=? AND status='running'`, run.ID); err != nil {
		return err
	}
	// A preparing run whose ownership was never saved can still own a branch
	// and a worktree from an interrupted creation. Release them now, so the
	// next run for the same issue can prepare instead of refusing them.
	if err := s.releasePreparation(ctx, repo, run); err != nil {
		return err
	}
	// Clear ready even if stop arrived before claim processing removed it.
	// Keep stop intent pending on failure so a terminal run cannot redispatch.
	if err := s.deps.GitHub.RemoveLabel(ctx, repo.Repo, run.IssueNumber, repo.Labels.Ready); err != nil {
		return err
	}
	if err := s.deps.GitHub.RemoveLabel(ctx, repo.Repo, run.IssueNumber, repo.Labels.Running); err != nil {
		return err
	}
	var hasWork bool
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE(worktree_path,'') != '' OR COALESCE(branch,'') != '' OR COALESCE(pr_number,0) != 0 FROM runs WHERE id=?", run.ID).Scan(&hasWork); err != nil {
		return err
	}
	if hasWork {
		if err := s.deps.GitHub.AddLabel(ctx, repo.Repo, run.IssueNumber, repo.Labels.NeedsAttention); err != nil {
			return err
		}
	}
	_, err := s.workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.Stop})
	return err
}

// releasePreparation releases the leftovers of an interrupted preparation, so
// the next run for the same issue can prepare instead of refusing them. Only
// a preparing run without saved ownership can own them: a run that saved its
// ownership keeps its branch and worktree, because Stop preserves the work of
// a run.
func (s *Scheduler) releasePreparation(ctx context.Context, repo config.Repository, run workflow.Run) error {
	if run.State != workflow.Preparing {
		return nil
	}
	owned, err := s.savedOwnership(ctx, run.ID)
	if err != nil {
		return err
	}
	if owned {
		return nil
	}
	leftovers, err := s.deps.Git.DiscardPreparation(ctx, managedgit.PrepareRequest{Repository: repo.Repo, BaseBranch: repo.BaseBranch, RunID: run.ID, IssueNumber: run.IssueNumber})
	if err != nil {
		return err
	}
	if !leftovers.Preserved {
		return nil
	}
	return s.preserveLeftovers(ctx, repo, run, leftovers)
}

// savedOwnership reports whether a run saved its Git ownership. A run without
// it owns no branch or worktree.
func (s *Scheduler) savedOwnership(ctx context.Context, id string) (bool, error) {
	var gitJSON sql.NullString
	if err := s.db.QueryRowContext(ctx, "SELECT git_json FROM scheduler_runs WHERE run_id=?", id).Scan(&gitJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if !gitJSON.Valid || gitJSON.String == "" {
		return false, nil
	}
	var gitRun managedgit.Run
	if err := json.Unmarshal([]byte(gitJSON.String), &gitRun); err != nil {
		return false, err
	}
	return gitRun.ID != "", nil
}

// preserveLeftovers records the leftovers an interrupted preparation left
// behind and names them on the issue that needs attention. The run keeps
// naming them, so the dashboard and the reconciliation know that the
// artifacts belong to it.
func (s *Scheduler) preserveLeftovers(ctx context.Context, repo config.Repository, run workflow.Run, leftovers managedgit.Leftovers) error {
	if _, err := s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		if _, err := tx.ExecContext(ctx, "UPDATE runs SET branch=?, worktree_path=? WHERE id=?", leftovers.Branch, leftovers.Path, run.ID); err != nil {
			return events.Draft{}, err
		}
		return events.Draft{RunID: run.ID, Type: "run.stop_leftovers", Payload: map[string]string{
			"branch": leftovers.Branch,
			"path":   leftovers.Path,
			"reason": leftovers.Reason,
		}}, nil
	}); err != nil {
		return err
	}
	return s.deps.GitHub.EnsureStopLeftoversComment(ctx, repo.Repo, run.IssueNumber, run.ID, leftovers.Branch, leftovers.Path, leftovers.Reason)
}

// Watch returns only the live attempt of the current phase. The CLI attaches
// with tmux's read-only client flag; completed and missing sessions are rejected.
func (s *Scheduler) Watch(ctx context.Context, id string) (runner.SessionRef, error) {
	var ref runner.SessionRef
	err := s.workflow.WithRunOperation(ctx, id, func(ctx context.Context, run workflow.Run) error {
		if run.State != workflow.Active && run.PendingHandback() == nil {
			return &fault.Error{Code: "phase.not_running", Message: "Run has no live automated phase"}
		}
		var input string
		err := s.db.QueryRowContext(ctx, `SELECT process_session,input_path FROM phase_attempts WHERE run_id=? AND phase=? AND status='running' ORDER BY started_at DESC,rowid DESC LIMIT 1`, id, run.Phase).Scan(&ref.Name, &input)
		if errors.Is(err, sql.ErrNoRows) {
			return &fault.Error{Code: "phase.not_running", Message: "Run has no live phase session"}
		}
		if err != nil {
			return err
		}
		ref.PhaseDir = filepath.Dir(input)
		status, err := s.deps.Runner.SessionStatus(ctx, ref)
		if err != nil {
			return err
		}
		if status.State != runner.SessionRunning {
			return &fault.Error{Code: "phase.not_running", Message: "Phase session has finished or is missing"}
		}
		return nil
	})
	return ref, err
}

func (s *Scheduler) stopOwnedPhases(ctx context.Context, run workflow.Run) error {
	rows, err := s.db.QueryContext(ctx, `SELECT process_session,input_path FROM phase_attempts WHERE run_id=? AND status='running'`, run.ID)
	if err != nil {
		return err
	}
	var refs []runner.SessionRef
	for rows.Next() {
		var name, input sql.NullString
		if err := rows.Scan(&name, &input); err != nil {
			rows.Close()
			return err
		}
		if name.Valid && name.String != "" {
			refs = append(refs, runner.SessionRef{Name: name.String, PhaseDir: filepath.Dir(input.String)})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if err := s.deps.Runner.StopSession(ctx, ref); err != nil {
			return err
		}
		status, err := s.deps.Runner.SessionStatus(ctx, ref)
		if err != nil {
			return err
		}
		if status.State == runner.SessionRunning {
			return &fault.Error{Code: "phase.stop_pending", Message: "Owned process is still running"}
		}
	}

	return nil
}
