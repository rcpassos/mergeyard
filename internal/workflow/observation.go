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

// RunObservation keeps lifecycle and current execution metadata from one SQL
// statement. Histories retain the existing, separately read evidence semantics.
type RunObservation struct {
	Run
	Branch, Worktree, Agent, SessionID, IssueJSON, PRURL string
	Model, Effort, ProcessSession, LogPath, SkillsJSON   string
	Attempt, Round                                       int
	StopRequested                                        bool
}

const runObservationQuery = `SELECT r.id,r.repository,r.issue_number,r.state,COALESCE(r.current_phase,''),
 r.created_at,r.updated_at,COALESCE(r.completed_at,''),COALESCE(r.last_error_code,''),COALESCE(r.last_error_message,''),
 COALESCE(r.pr_number,0),r.review_round,COALESCE(r.approved_sha,''),r.takeover_status,COALESCE(r.human_action_json,'null'),COALESCE(r.role_settings_json,'null'),
 COALESCE(r.branch,''),COALESCE(r.worktree_path,''),
 CASE WHEN r.current_phase='review' THEN COALESCE(r.reviewer_agent,'') ELSE COALESCE(r.implementer_agent,'') END,
 CASE WHEN r.current_phase='review' THEN COALESCE(r.reviewer_session_id,'') ELSE COALESCE(r.implementer_session_id,'') END,
 COALESCE(s.issue_json,'{}'),COALESCE(s.pr_url,''),COALESCE(a.attempt,0),COALESCE(a.round,0),
 COALESCE(a.model,''),COALESCE(a.effort,''),COALESCE(a.process_session,''),COALESCE(a.log_path,''),COALESCE(a.skills_json,''),r.stop_requested
 FROM runs r LEFT JOIN scheduler_runs s ON s.run_id=r.id
 LEFT JOIN phase_attempts a ON a.id=(SELECT id FROM phase_attempts WHERE run_id=r.id AND phase=r.current_phase ORDER BY started_at DESC,rowid DESC,round DESC,attempt DESC LIMIT 1)`

func scanRunObservation(row interface{ Scan(...any) error }) (RunObservation, error) {
	var v RunObservation
	var humanAction, roleSettings string
	err := row.Scan(&v.ID, &v.Repository, &v.IssueNumber, &v.State, &v.Phase,
		&v.CreatedAt, &v.UpdatedAt, &v.CompletedAt, &v.LastErrorCode, &v.LastErrorMessage,
		&v.PRNumber, &v.ReviewRound, &v.ApprovedSHA, &v.TakeoverStatus, &humanAction, &roleSettings,
		&v.Branch, &v.Worktree, &v.Agent, &v.SessionID, &v.IssueJSON, &v.PRURL, &v.Attempt, &v.Round,
		&v.Model, &v.Effort, &v.ProcessSession, &v.LogPath, &v.SkillsJSON, &v.StopRequested)
	if err != nil {
		return RunObservation{}, err
	}
	if err := json.Unmarshal([]byte(humanAction), &v.HumanAction); err != nil {
		return RunObservation{}, err
	}
	if err := json.Unmarshal([]byte(roleSettings), &v.RoleSettings); err != nil {
		return RunObservation{}, err
	}
	return v, nil
}

// terminalRunStates is the SQL list of terminal run states. A terminal run
// whose merge cleanup is still pending has not left the Queue.
const terminalRunStates = `'FAILED','STOPPED','COMPLETED'`

// ObserveRecentRuns returns active runs plus the latest 20 terminal runs, or a
// selected run regardless of age. The terminal window also carries merge
// cleanup that is still pending, which Needs attention shows. Selection,
// ordering, lifecycle and execution metadata share one statement; no snapshot
// transaction is introduced.
func ObserveRecentRuns(ctx context.Context, db queryer, selectedID string) ([]RunObservation, error) {
	query := runObservationQuery
	var args []any
	if selectedID != "" {
		query += " WHERE r.id=?"
		args = append(args, selectedID)
	} else {
		query += ` WHERE r.state NOT IN (` + terminalRunStates + `) OR r.id IN
 (SELECT id FROM runs WHERE state IN (` + terminalRunStates + `) ORDER BY updated_at DESC,id LIMIT 20)`
	}
	return observeRuns(ctx, db, query+" ORDER BY r.updated_at DESC,r.id", args...)
}

// ObserveEndedRuns returns every ended run, newest first, for the full
// ended-run history. A terminal run whose merge cleanup is still pending has
// not ended and remains in Needs attention. The recent selection stays bounded
// for the Dashboard.
func ObserveEndedRuns(ctx context.Context, db queryer) ([]RunObservation, error) {
	observations, err := observeRuns(ctx, db, runObservationQuery+` WHERE r.state IN (`+terminalRunStates+`) ORDER BY r.updated_at DESC,r.id`)
	if err != nil {
		return nil, err
	}
	ended := observations[:0]
	for _, observation := range observations {
		if observation.Ended() {
			ended = append(ended, observation)
		}
	}
	return ended, nil
}

// observeRuns scans a run selection and then loads each run's evidence.
func observeRuns(ctx context.Context, db queryer, query string, args ...any) ([]RunObservation, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, storageError(err)
	}
	defer rows.Close()
	var observations []RunObservation
	for rows.Next() {
		v, err := scanRunObservation(rows)
		if err != nil {
			return nil, storageError(err)
		}
		observations = append(observations, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, storageError(err)
	}
	// Release the selection connection before loading evidence, including when
	// the caller's SQLite pool has only one connection.
	for i := range observations {
		observations[i].Run, err = observeRunEvidence(ctx, db, observations[i].Run)
		if err != nil {
			return nil, err
		}
	}
	return observations, nil
}

// ObserveRun assembles persisted lifecycle facts and evidence for one run.
// It accepts a database or an existing transaction and preserves the caller's
// read consistency; it does not introduce a snapshot transaction.
func ObserveRun(ctx context.Context, db queryer, id string) (Run, error) {
	v, err := scanRunObservation(db.QueryRowContext(ctx, runObservationQuery+" WHERE r.id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, &fault.Error{Code: "internal.run_not_found", Message: "Run does not exist", Path: id, Err: err}
	}
	if err != nil {
		return Run{}, storageError(err)
	}
	return observeRunEvidence(ctx, db, v.Run)
}

func observeRunEvidence(ctx context.Context, db queryer, run Run) (Run, error) {
	id := run.ID
	var err error
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
