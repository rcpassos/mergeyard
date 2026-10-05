package workflow

import "context"

// SessionRecovery keeps the warning and prior diagnostic after successful work.
// SessionID is the role's active identity, including discovery after restart.
type SessionRecovery struct {
	Phase             Phase  `json:"phase"`
	Role              string `json:"role"`
	Round             int    `json:"round"`
	PreviousSessionID string `json:"previous_session_id"`
	SessionID         string `json:"session_id"`
	Error             string `json:"error"`
}

func LoadSessionRecoveries(ctx context.Context, db queryer, id string) ([]SessionRecovery, error) {
	rows, err := db.QueryContext(ctx, `SELECT c.phase,c.role,c.round,c.previous_session_id,CASE c.role WHEN 'reviewer' THEN COALESCE(r.reviewer_session_id,'') ELSE COALESCE(r.implementer_session_id,'') END,COALESCE(a.error,'') FROM session_recoveries c JOIN runs r ON r.id=c.run_id JOIN phase_attempts a ON a.id=c.failed_attempt_id WHERE c.run_id=? ORDER BY c.created_at,c.phase,c.round`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []SessionRecovery
	for rows.Next() {
		var v SessionRecovery
		if err := rows.Scan(&v.Phase, &v.Role, &v.Round, &v.PreviousSessionID, &v.SessionID, &v.Error); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
