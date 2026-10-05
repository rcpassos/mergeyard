package review

import "context"

// Publication is a durable comment receipt or a pending publication warning.
type Publication struct {
	AttemptID   string `json:"attempt_id"`
	Phase       string `json:"phase"`
	Round       int    `json:"round"`
	Attempt     int    `json:"attempt"`
	State       string `json:"state"`
	Attempts    int    `json:"attempts"`
	CommentID   int64  `json:"comment_id,omitempty"`
	CommentURL  string `json:"comment_url,omitempty"`
	WarningCode string `json:"warning_code,omitempty"`
	Warning     string `json:"warning,omitempty"`
}

func LoadPublications(ctx context.Context, db queryer, runID string) ([]Publication, error) {
	rows, err := db.QueryContext(ctx, `SELECT attempt_id,phase,round,attempt,state,attempts,COALESCE(comment_id,0),COALESCE(comment_url,''),warning_code,warning FROM report_publications WHERE run_id=? ORDER BY round,CASE phase WHEN 'review' THEN 0 ELSE 1 END,attempt`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Publication
	for rows.Next() {
		var p Publication
		if err := rows.Scan(&p.AttemptID, &p.Phase, &p.Round, &p.Attempt, &p.State, &p.Attempts, &p.CommentID, &p.CommentURL, &p.WarningCode, &p.Warning); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}
