package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"

	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// Observe the existing capture, including after an offline finish. No new
// process or session is created to recover identity after a control-plane restart.
func (s *Scheduler) discoverImplementSession(ctx context.Context, id string, a *attempt) error {
	return s.discoverSession(ctx, id, workflow.Implement, a)
}

func (s *Scheduler) discoverSession(ctx context.Context, id string, phase workflow.Phase, a *attempt) error {
	observer, ok := s.harnesses[a.agent].(harness.SessionDiscoverer)
	if !ok {
		return nil
	}
	data, err := s.deps.Runner.ReadFile(ctx, filepath.Join(a.ref.PhaseDir, "events.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	identity, err := observer.DiscoverSession(data)
	if err != nil {
		return err
	}
	if identity == "" {
		return nil
	}

	var otherIdentity string
	role := phase.Role()
	otherRole := workflow.Review.Role()
	if role == otherRole {
		otherRole = workflow.Implement.Role()
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE("+otherRole+"_session_id,'') FROM runs WHERE id=?", id).Scan(&otherIdentity); err != nil {
		return err
	}
	if identity == otherIdentity {
		return &fault.Error{Code: "harness.session_identity_invalid", Message: "Harness reused the other role's conversation identity"}
	}
	if a.sessionID != "" {
		if identity != a.sessionID {
			return &fault.Error{Code: "harness.session_identity_invalid", Message: "Codex emitted a different identity than the persisted role session"}
		}
		return nil
	}

	table := ""
	if phase == workflow.Review {
		table = "review_attempts"
	}
	if phase == workflow.Fix {
		table = "fix_attempts"
	}
	_, err = s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		_, err := tx.ExecContext(ctx, "UPDATE runs SET "+role+"_session_id=?,updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?", identity, id)

		if err == nil {
			_, err = tx.ExecContext(ctx, "UPDATE phase_attempts SET session_id=? WHERE id=?", identity, a.id)
		}
		if err == nil && table != "" {
			_, err = tx.ExecContext(ctx, "UPDATE "+table+" SET session_id=? WHERE attempt_id=?", identity, a.id)
		}
		return events.Draft{RunID: id, Type: "harness.session_discovered", Payload: map[string]any{"agent": a.agent, "session_id": identity, "phase": phase, "attempt": a.number}}, err
	})
	if err == nil {
		a.sessionID = identity
	}
	return err
}

// Identity observation is local and does not wait for the GitHub polling interval.
// It shares the tick and per-run gates with reconciliation and lifecycle controls.
func (s *Scheduler) observeIdentities(ctx context.Context) error {
	s.tick.Lock()
	defer s.tick.Unlock()
	runs, err := s.Runs(ctx)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.State != workflow.Active {
			continue
		}
		err := s.workflow.WithRunOperation(ctx, run.ID, func(ctx context.Context, current workflow.Run) error {
			if current.State != workflow.Active {
				return nil
			}
			var a attempt
			var err error
			switch current.Phase {
			case workflow.Implement:
				a, err = s.lastAttempt(ctx, current.ID)
			case workflow.Review:
				var review reviewAttempt
				review, err = s.lastReview(ctx, current)
				a = review.attempt
			case workflow.Fix:
				var fix fixAttempt
				fix, err = s.lastFix(ctx, current)
				a = fix.attempt
			default:
				return nil
			}
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if a.status != "running" || a.sessionID != "" {
				return nil
			}
			return s.discoverSession(ctx, current.ID, current.Phase, &a)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func invalidSessionDiscovery(err error) bool {
	var coded *fault.Error
	return errors.As(err, &coded) && (coded.Code == "phase.result_invalid" || coded.Code == "harness.session_identity_invalid" || coded.Code == "harness.session_resume_failed")
}
