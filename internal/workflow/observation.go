package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/escalation"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/maintenance"
	"github.com/rcpassos/mergeyard/internal/review"
)

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// ObserveRun assembles persisted lifecycle facts and evidence for one run.
// It accepts a database or an existing transaction and preserves the caller's
// read consistency; it does not introduce a snapshot transaction.
func ObserveRun(ctx context.Context, db queryer, id string) (Run, error) {
	var run Run
	var humanAction, roleSettings string
	err := db.QueryRowContext(ctx, `SELECT id, repository, issue_number, state, COALESCE(current_phase, ''),
		created_at, updated_at, COALESCE(completed_at, ''), COALESCE(last_error_code, ''), COALESCE(last_error_message, ''),
		COALESCE(pr_number, 0), review_round, COALESCE(approved_sha, ''), takeover_status, COALESCE(human_action_json, 'null'), COALESCE(role_settings_json, 'null')
		FROM runs WHERE id = ?`, id).Scan(&run.ID, &run.Repository, &run.IssueNumber, &run.State, &run.Phase,
		&run.CreatedAt, &run.UpdatedAt, &run.CompletedAt, &run.LastErrorCode, &run.LastErrorMessage,
		&run.PRNumber, &run.ReviewRound, &run.ApprovedSHA, &run.TakeoverStatus, &humanAction, &roleSettings)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, &fault.Error{Code: "internal.run_not_found", Message: "Run does not exist", Path: id, Err: err}
	}
	if err != nil {
		return Run{}, storageError(err)
	}
	if err := json.Unmarshal([]byte(humanAction), &run.HumanAction); err != nil {
		return Run{}, storageError(err)
	}
	if err := json.Unmarshal([]byte(roleSettings), &run.RoleSettings); err != nil {
		return Run{}, storageError(err)
	}
	run.AgentPorts, err = loadAgentPorts(ctx, db, id)
	if err != nil {
		return Run{}, storageError(err)
	}
	run.CreditProbes, err = LoadCreditProbes(ctx, db, id)
	if err != nil {
		return Run{}, storageError(err)
	}
	run.HarnessWaitHistory, err = LoadHarnessWaits(ctx, db, id)
	if err != nil {
		return Run{}, storageError(err)
	}
	run.HarnessWait, err = CurrentHarnessWait(ctx, db, run.HarnessWaitHistory)
	if err != nil {
		return Run{}, storageError(err)
	}
	run.Merge, err = maintenance.Load(ctx, db, id)
	if err != nil {
		return Run{}, storageError(err)
	}
	run.Handbacks, err = LoadHandbacks(ctx, db, id)
	if err != nil {
		return Run{}, storageError(err)
	}
	run.Retries, err = LoadRetries(ctx, db, id)
	if err != nil {
		return Run{}, storageError(err)
	}
	run.CI, err = ci.Load(ctx, db, id)
	if err != nil {
		return Run{}, storageError(err)
	}
	run.Escalation, err = escalation.Load(ctx, db, id)
	if err != nil {
		return Run{}, storageError(err)
	}
	run.SessionRecoveries, err = LoadSessionRecoveries(ctx, db, id)
	if err != nil {
		return Run{}, storageError(err)
	}
	run.SystemSteps, err = LoadSystemSteps(ctx, db, id)
	if err != nil {
		return Run{}, storageError(err)
	}
	run.Implementer, err = LoadImplementSnapshot(ctx, db, id)
	if err != nil {
		return Run{}, storageError(err)
	}
	run.ImplementHistory, err = LoadImplementHistory(ctx, db, id)
	if err != nil {
		return Run{}, storageError(err)
	}

	run.FixHistory, err = review.LoadFixHistory(ctx, db, id)
	if err != nil {
		return Run{}, storageError(err)
	}
	if len(run.FixHistory) > 0 {
		run.Fix = &run.FixHistory[len(run.FixHistory)-1]
	}
	run.Publications, err = review.LoadPublications(ctx, db, id)
	if err != nil {
		return Run{}, storageError(err)
	}
	run.ReviewHistory, err = review.LoadHistory(ctx, db, id)
	if err != nil {
		return Run{}, storageError(err)
	}
	run.Review, err = review.LoadSnapshot(ctx, db, id)
	if err != nil {
		return Run{}, storageError(err)
	}
	return run, nil
}
