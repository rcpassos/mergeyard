package escalation

import (
	"context"
	"database/sql"
	"encoding/json"
)

// Snapshot ties saved reasons and publication receipts to the evaluated diff.
type Snapshot struct {
	BaseSHA            string `json:"base_sha"`
	HeadSHA            string `json:"head_sha"`
	Reasons            []Flag `json:"reasons"`
	EvaluationWarning  string `json:"evaluation_warning,omitempty"`
	PublicationWarning string `json:"publication_warning,omitempty"`
	LastAttemptAt      string `json:"last_attempt_at,omitempty"`
	Attempts           int    `json:"attempts,omitempty"`
	Failed             bool   `json:"failed,omitempty"`
	Stopped            bool   `json:"stopped,omitempty"`
	Published          bool   `json:"published"`
}

type Queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func Load(ctx context.Context, db Queryer, id string) (*Snapshot, error) {
	var data string
	if err := db.QueryRowContext(ctx, "SELECT COALESCE(escalation_json,'null') FROM runs WHERE id=?", id).Scan(&data); err != nil {
		return nil, err
	}
	var snapshot *Snapshot
	err := json.Unmarshal([]byte(data), &snapshot)
	return snapshot, err
}

func Save(ctx context.Context, tx *sql.Tx, id string, snapshot Snapshot) error {
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE runs SET escalation_json=? WHERE id=?", string(data), id)
	return err
}
