// Package maintenance journals manual merge observation and independent cleanup.
package maintenance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

type Snapshot struct {
	ObservedAt       string `json:"observed_at"`
	Early            bool   `json:"early"`
	ProcessExited    bool   `json:"process_exited"`
	ReadyLabel       string `json:"ready_label"`
	RunningLabel     string `json:"running_label"`
	AttentionLabel   string `json:"attention_label"`
	ReadyRemoved     bool   `json:"ready_removed"`
	RunningRemoved   bool   `json:"running_removed"`
	AttentionRemoved bool   `json:"attention_removed"`
	IssueClosed      bool   `json:"issue_closed"`
	ReviewRestored   bool   `json:"review_restored"`
	WorktreeStarted  bool   `json:"worktree_started"`
	WorktreeRemoved  bool   `json:"worktree_removed"`
	BranchDeleted    bool   `json:"branch_deleted"`
	Error            string `json:"error,omitempty"`
}

func (s Snapshot) Pending() bool { return len(s.Remaining()) > 0 }
func (s Snapshot) Remaining() []string {
	var actions []string
	for _, step := range []struct {
		done   bool
		action string
	}{
		{s.ProcessExited, "Stop owned phase and verify exit"},
		{s.ReadyRemoved, "Remove ready label"}, {s.RunningRemoved, "Remove running label"}, {s.AttentionRemoved, "Remove attention label"},
		{s.IssueClosed, "Close issue if still open"}, {s.ReviewRestored, "Restore independent review workspace"},
		{s.WorktreeRemoved, "Remove clean worktree and prune metadata"}, {s.BranchDeleted, "Delete local branch"},
	} {
		if !step.done {
			actions = append(actions, step.action)
		}
	}
	return actions
}

type Queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func Load(ctx context.Context, db Queryer, id string) (*Snapshot, error) {
	var data string
	err := db.QueryRowContext(ctx, "SELECT snapshot_json FROM merge_cleanup WHERE run_id=?", id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s Snapshot
	err = json.Unmarshal([]byte(data), &s)
	return &s, err
}
func Save(ctx context.Context, tx *sql.Tx, id string, s Snapshot) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO merge_cleanup(run_id,snapshot_json) VALUES(?,?) ON CONFLICT(run_id) DO UPDATE SET snapshot_json=excluded.snapshot_json`, id, string(data))
	return err
}
