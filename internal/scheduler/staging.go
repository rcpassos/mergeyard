package scheduler

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/rcpassos/mergeyard/internal/events"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// Retain staging evidence even when excluded-only work returns git.no_changes.
// The event exposes warnings in the run timeline; the file belongs to the
// concrete phase attempt alongside the harness's logs and result.
func (s *Scheduler) recordStagingDiagnostics(ctx context.Context, run workflow.Run, result managedgit.CommitResult) error {
	if len(result.Diagnostics) == 0 {
		return nil
	}
	var input string
	var attempt int
	err := s.db.QueryRowContext(ctx, `SELECT input_path,attempt FROM phase_attempts WHERE run_id=? AND phase=? AND round=? AND step='' ORDER BY attempt DESC LIMIT 1`, run.ID, run.Phase, phaseRound(run, run.Phase)).Scan(&input, &attempt)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(result.Diagnostics, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(filepath.Dir(input), "staging.json")
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		return err
	}
	_, err = s.bus.Publish(ctx, events.Draft{RunID: run.ID, Type: "git.staging_diagnostics", Payload: map[string]any{
		"phase": run.Phase, "round": phaseRound(run, run.Phase), "attempt": attempt, "path": path, "diagnostics": result.Diagnostics,
	}})
	return err
}
