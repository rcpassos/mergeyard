package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"

	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// Stop interrupts the phase and preserves all code. Persisting intent first
// prevents a failed label write or restart from relaunching interrupted work.
func (s *Scheduler) Stop(ctx context.Context, id string) error {
	return s.workflow.WithRunOperation(ctx, id, func(ctx context.Context, run workflow.Run) error {
		if run.State == workflow.Stopped {
			return nil
		}
		if run.State.Terminal() {
			return &fault.Error{Code: "run.terminal", Message: "Run is already terminal"}
		}
		if _, ok := s.repository(run.Repository); !ok {
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
	repo, ok := s.repository(run.Repository)
	if !ok {
		return &fault.Error{Code: "config.repository_missing", Message: "Run repository is no longer configured"}
	}
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
	_, err = s.workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.Stop})
	return err
}

// Watch returns only the live attempt of the current phase. The CLI attaches
// with tmux's read-only client flag; completed and missing sessions are rejected.
func (s *Scheduler) Watch(ctx context.Context, id string) (runner.SessionRef, error) {
	var ref runner.SessionRef
	err := s.workflow.WithRunOperation(ctx, id, func(ctx context.Context, run workflow.Run) error {
		if run.State != workflow.Active {
			return &fault.Error{Code: "phase.not_running", Message: "Run has no live automated phase"}
		}
		var input string
		err := s.db.QueryRowContext(ctx, `SELECT process_session,input_path FROM phase_attempts WHERE run_id=? AND phase=? AND status='running' ORDER BY round DESC,attempt DESC LIMIT 1`, id, run.Phase).Scan(&ref.Name, &input)
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
