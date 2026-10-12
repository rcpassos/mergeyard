package scheduler

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type attentionPublication struct {
	eventID                          int64
	runID, repository, body, warning string
	number                           int
}

// Each attention event freezes its own comment. Later Retry, Stop, or restart
// cannot erase the decision or duplicate an ambiguously successful remote write.
func (s *Scheduler) publishAttention(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT e.id,e.payload_json FROM events e LEFT JOIN attention_publications p ON p.event_id=e.id WHERE e.type='run.needs_attention' AND p.event_id IS NULL ORDER BY e.id`)
	if err != nil {
		return err
	}
	var queue []attentionPublication
	for rows.Next() {
		var id int64
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return err
		}
		var event struct {
			Run workflow.Run `json:"run"`
		}
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			rows.Close()
			return err
		}
		run := event.Run
		// Historical events predate human actions and do not need new comments.
		if run.HumanAction == nil {
			continue
		}
		number := run.IssueNumber
		if run.PRNumber > 0 {
			number = run.PRNumber
		}
		encoded, err := json.MarshalIndent(struct {
			Code        string             `json:"code"`
			Message     string             `json:"message"`
			HumanAction *fault.HumanAction `json:"human_action"`
		}{run.LastErrorCode, run.LastErrorMessage, run.HumanAction}, "", "  ")
		if err != nil {
			rows.Close()
			return err
		}
		identity := sha256.Sum256([]byte(fmt.Sprintf("attention:%s:%d", run.ID, id)))
		body := fmt.Sprintf("<!-- mergeyard:report:%x -->\n## Human action required\n\nRun %s needs attention. Answer the decision or perform the command in human_action below before retrying.\n\n```json\n%s\n```", identity, run.ID, strings.ReplaceAll(string(encoded), "`", `\u0060`))
		queue = append(queue, attentionPublication{eventID: id, runID: run.ID, repository: run.Repository, number: number, body: body})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, p := range queue {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO attention_publications(event_id,run_id,repository,issue_number,body) VALUES(?,?,?,?,?)`, p.eventID, p.runID, p.repository, p.number, p.body); err != nil {
			return err
		}
	}
	if !s.cfg.PRComments {
		return nil
	}
	rows, err = s.db.QueryContext(ctx, `SELECT event_id,run_id,repository,issue_number,body,warning_code FROM attention_publications WHERE state='pending' ORDER BY COALESCE(last_attempt_at,''),event_id`)
	if err != nil {
		return err
	}
	var pending []attentionPublication
	for rows.Next() {
		var p attentionPublication
		if err := rows.Scan(&p.eventID, &p.runID, &p.repository, &p.number, &p.body, &p.warning); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	deliveries := make([]commentDelivery, 0, len(pending))
	for _, p := range pending {
		deliveries = append(deliveries, p.delivery(s))
	}
	return s.publishComments(ctx, deliveries)
}

func (p attentionPublication) delivery(s *Scheduler) commentDelivery {
	return commentDelivery{
		repository: p.repository, number: p.number, body: p.body, warningCode: p.warning,
		startAttempt: func(ctx context.Context) error {
			_, err := s.db.ExecContext(ctx, `UPDATE attention_publications SET attempts=attempts+1,last_attempt_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE event_id=?`, p.eventID)
			return err
		},
		saveWarning: func(ctx context.Context, tx *sql.Tx, code string) (events.Draft, error) {
			_, err := tx.ExecContext(ctx, `UPDATE attention_publications SET warning_code=? WHERE event_id=?`, code, p.eventID)
			return events.Draft{RunID: p.runID, Type: "publication.warning", Payload: map[string]any{"attention_event_id": p.eventID, "code": code, "message": "Human action is saved locally. Attention comment publication will retry."}}, err
		},
		saveReceipt: func(ctx context.Context, tx *sql.Tx, comment *github.Comment) (events.Draft, error) {
			_, err := tx.ExecContext(ctx, `UPDATE attention_publications SET state='published',comment_id=?,comment_url=?,warning_code='' WHERE event_id=?`, comment.ID, comment.URL, p.eventID)
			return events.Draft{RunID: p.runID, Type: "publication.published", Payload: map[string]any{"attention_event_id": p.eventID, "comment_id": comment.ID, "comment_url": comment.URL}}, err
		},
	}
}
