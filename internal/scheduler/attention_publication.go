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
	lastAttempt                      string
	number, attempts                 int
	finished, failed                 bool
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
	rows, err = s.db.QueryContext(ctx, `SELECT p.event_id,p.run_id,p.repository,p.issue_number,p.body,p.warning_code,p.attempts,COALESCE(p.last_attempt_at,''),r.state,p.failed
		FROM attention_publications p JOIN runs r ON r.id=p.run_id
		WHERE p.state='pending' AND p.stopped=0
		ORDER BY COALESCE(p.last_attempt_at,''),p.event_id`)
	if err != nil {
		return err
	}
	var loaded []attentionPublication
	for rows.Next() {
		var p attentionPublication
		var runState string
		if err := rows.Scan(&p.eventID, &p.runID, &p.repository, &p.number, &p.body, &p.warning, &p.attempts, &p.lastAttempt, &runState, &p.failed); err != nil {
			rows.Close()
			return err
		}
		p.finished = workflow.State(runState).Terminal()
		loaded = append(loaded, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var pending []attentionPublication
	for _, p := range loaded {
		state := publicationState{Attempts: p.attempts, LastAttemptAt: p.lastAttempt, Finished: p.finished, Failed: p.failed}
		disposition, err := state.disposition(s.deps.Now())
		if err != nil {
			return err
		}
		switch disposition {
		case publicationStop:
			warning := stoppedPublicationWarning("Human action is saved locally. This run is finished, so attention comment publication will not retry.")
			payload := map[string]any{"attention_event_id": p.eventID, "code": p.warning, "message": warning}
			if err := s.stopPublication(ctx, p.runID, payload, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `UPDATE attention_publications SET stopped=1,warning=? WHERE event_id=?`, warning, p.eventID)
				return err
			}); err != nil {
				return err
			}
		case publicationReady:
			pending = append(pending, p)
		}
	}
	deliveries := make([]commentDelivery, 0, len(pending))
	for _, p := range pending {
		deliveries = append(deliveries, p.delivery(s))
	}
	return s.publishComments(ctx, deliveries)
}

func (p attentionPublication) delivery(s *Scheduler) commentDelivery {
	return commentDelivery{
		runID: p.runID, repository: p.repository, number: p.number, body: p.body, warningCode: p.warning, finished: p.finished,
		warnings: publicationWarnings{
			Deferred: "Human action is saved locally. Attention comment publication is deferred.",
			Terminal: stoppedPublicationWarning("Human action is saved locally. Attention comment publication cannot deliver this comment."),
			Finished: stoppedPublicationWarning("Human action is saved locally. This run is finished, so attention comment publication will not retry."),
		},
		startAttempt: func(ctx context.Context) error {
			_, err := s.db.ExecContext(ctx, `UPDATE attention_publications SET attempts=attempts+1,last_attempt_at=?,failed=0 WHERE event_id=?`, publicationTimestamp(s.deps.Now()), p.eventID)
			return err
		},
		saveWarning: func(ctx context.Context, tx *sql.Tx, outcome publicationOutcome) (events.Draft, error) {
			_, err := tx.ExecContext(ctx, `UPDATE attention_publications SET warning_code=?,warning=?,failed=?,stopped=? WHERE event_id=?`, outcome.Code, outcome.Message, outcome.Failed, outcome.Stopped, p.eventID)
			return events.Draft{RunID: p.runID, Type: "publication.warning", Payload: map[string]any{"attention_event_id": p.eventID, "code": outcome.Code, "message": outcome.Message}}, err
		},
		saveAttemptResult: func(ctx context.Context, tx *sql.Tx, outcome publicationOutcome) error {
			exec := s.db.ExecContext
			if tx != nil {
				exec = tx.ExecContext
			}
			if !outcome.Stopped {
				_, err := exec(ctx, `UPDATE attention_publications SET failed=?,stopped=0 WHERE event_id=?`, outcome.Failed, p.eventID)
				return err
			}
			_, err := exec(ctx, `UPDATE attention_publications SET failed=?,stopped=1,warning=? WHERE event_id=?`, outcome.Failed, outcome.Message, p.eventID)
			return err
		},
		stopPayload: func(outcome publicationOutcome) map[string]any {
			return map[string]any{"attention_event_id": p.eventID, "code": outcome.Code, "message": outcome.Message}
		},
		saveReceipt: func(ctx context.Context, tx *sql.Tx, comment *github.Comment) (events.Draft, error) {
			_, err := tx.ExecContext(ctx, `UPDATE attention_publications SET state='published',comment_id=?,comment_url=?,warning_code='',warning='',failed=0,stopped=0 WHERE event_id=?`, comment.ID, comment.URL, p.eventID)
			return events.Draft{RunID: p.runID, Type: "publication.published", Payload: map[string]any{"attention_event_id": p.eventID, "comment_id": comment.ID, "comment_url": comment.URL}}, err
		},
	}
}
