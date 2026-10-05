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
	if a.sessionID != "" {
		if identity != a.sessionID {
			return &fault.Error{Code: "harness.session_resume_failed", Message: "Codex emitted a different identity than the persisted implementer session"}
		}
		return nil
	}
	_, err = s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		_, err := tx.ExecContext(ctx, "UPDATE runs SET implementer_session_id=?,updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?", identity, id)
		return events.Draft{RunID: id, Type: "harness.session_discovered", Payload: map[string]any{"agent": a.agent, "session_id": identity, "phase": "implement", "attempt": a.number}}, err
	})
	if err == nil {
		a.sessionID = identity
	}
	return err
}

// Identity observation is local and does not wait for the GitHub polling interval.
// It shares the tick and per-run gates with reconciliation and lifecycle controls.
func (s *Scheduler) observeImplementIdentities(ctx context.Context) error {
	s.tick.Lock()
	defer s.tick.Unlock()
	runs, err := s.Runs(ctx)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.State != workflow.Active || run.Phase != workflow.Implement || run.Implementer == nil || run.Implementer.Status != "running" || run.Implementer.SessionID != "" {
			continue
		}
		if _, ok := s.harnesses[run.Implementer.Agent].(harness.SessionDiscoverer); !ok {
			continue
		}
		err := s.workflow.WithRunOperation(ctx, run.ID, func(ctx context.Context, current workflow.Run) error {
			if current.State != workflow.Active || current.Phase != workflow.Implement {
				return nil
			}
			a, err := s.lastAttempt(ctx, current.ID)
			if err != nil {
				return err
			}
			if a.status != "running" {
				return nil
			}
			return s.discoverImplementSession(ctx, current.ID, &a)
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
