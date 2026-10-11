package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
)

// SystemCommandResult records one configured check command. Commands after the
// first failure are skipped; a missing exit marker on a finished step is a failure.
type SystemCommandResult struct {
	Command  string `json:"command"`
	Status   string `json:"status"`
	Output   string `json:"output,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
}

// SystemStepSnapshot records configured command execution within its owning phase.
// Its physical attempt count is independent of the agent's attempt budget.
type SystemStepSnapshot struct {
	OwnerID          string                `json:"owner_id,omitempty"`
	ID               string                `json:"id"`
	Phase            Phase                 `json:"phase"`
	Step             string                `json:"step"`
	Round            int                   `json:"round"`
	Attempt          int                   `json:"attempt"`
	Commands         []string              `json:"commands"`
	Results          []SystemCommandResult `json:"results,omitempty"`
	CommandTimeout   string                `json:"command_timeout"`
	WorktreeIdentity string                `json:"worktree_identity,omitempty"`
	TargetSHA        string                `json:"target_sha,omitempty"`
	Status           string                `json:"status"`
	ProcessSession   string                `json:"process_session"`
	PhaseDir         string                `json:"phase_dir"`
	TimedOut         bool                  `json:"timed_out,omitempty"`
	ExitCode         *int                  `json:"exit_code,omitempty"`
	Output           string                `json:"output,omitempty"`
	Error            string                `json:"error,omitempty"`
}

func LoadSystemSteps(ctx context.Context, db queryer, id string) ([]SystemStepSnapshot, error) {
	rows, err := db.QueryContext(ctx, `SELECT system_step_json,status,exit_code,COALESCE(error,'') FROM phase_attempts WHERE run_id=? AND step!='' ORDER BY rowid`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []SystemStepSnapshot
	for rows.Next() {
		var data, status, failure string
		var exit sql.NullInt64
		if err := rows.Scan(&data, &status, &exit, &failure); err != nil {
			return nil, err
		}
		var v SystemStepSnapshot
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			return nil, err
		}
		v.Status, v.Error = status, failure
		if exit.Valid {
			code := int(exit.Int64)
			v.ExitCode = &code
		}
		results = append(results, v)
	}
	return results, rows.Err()
}
