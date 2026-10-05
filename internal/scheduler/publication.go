package scheduler

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
)

// ReportPublisher performs inspect-before-create, including after ambiguous writes.
type ReportPublisher interface {
	EnsureReportComment(context.Context, string, int, string) (*github.Comment, error)
}

type pendingReport struct {
	id, runID, repository, phase, body, warning string
	pr, round, attempt                          int
}

// queueReports reconstructs missing publication intents from locally saved
// reports, including terminal runs. Frozen bodies are never regenerated on replay.
func (s *Scheduler) queueReports(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT a.id,a.run_id,r.repository,r.pr_number,a.phase,a.round,a.attempt,COALESCE(v.report_json,f.report_json),COALESCE(v.accepted,0)
FROM phase_attempts a JOIN runs r ON r.id=a.run_id
LEFT JOIN review_attempts v ON v.attempt_id=a.id LEFT JOIN fix_attempts f ON f.attempt_id=a.id
LEFT JOIN report_publications p ON p.attempt_id=a.id
WHERE p.attempt_id IS NULL AND r.pr_number>0 AND COALESCE(v.report_json,f.report_json) IS NOT NULL
ORDER BY a.run_id,a.round,CASE a.phase WHEN 'review' THEN 0 ELSE 1 END,a.attempt`)
	if err != nil {
		return err
	}
	var reports []pendingReport
	for rows.Next() {
		var p pendingReport
		var raw string
		var accepted bool
		if err := rows.Scan(&p.id, &p.runID, &p.repository, &p.pr, &p.phase, &p.round, &p.attempt, &raw, &accepted); err != nil {
			rows.Close()
			return err
		}
		// JSON fences keep model text literal: backticks cannot terminate the
		// fence, and the JSON encoder escapes HTML and ownership-marker text.
		var report any
		if err := json.Unmarshal([]byte(raw), &report); err != nil {
			rows.Close()
			return err
		}
		var acceptedReview *bool
		if p.phase == "review" {
			acceptedReview = &accepted
		}
		encoded, err := json.MarshalIndent(struct {
			RunID     string `json:"run_id"`
			AttemptID string `json:"report_id"`
			Accepted  *bool  `json:"accepted,omitempty"`
			Report    any    `json:"report"`
		}{p.runID, p.id, acceptedReview, report}, "", "  ")
		if err != nil {
			rows.Close()
			return err
		}
		title := "Review"
		if p.phase == "fix" {
			title = "Fix"
		}
		identity := sha256.Sum256([]byte(p.runID + "\x00" + p.phase + "\x00" + p.id))
		p.body = fmt.Sprintf("<!-- mergeyard:report:%x -->\n## %s report · round %d, attempt %d\n\n```json\n%s\n```", identity, title, p.round, p.attempt, strings.ReplaceAll(string(encoded), "`", `\u0060`))
		reports = append(reports, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, p := range reports {
		_, err := s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
			_, err := tx.ExecContext(ctx, `INSERT INTO report_publications(attempt_id,run_id,repository,pr_number,phase,round,attempt,body) VALUES (?,?,?,?,?,?,?,?)`, p.id, p.runID, p.repository, p.pr, p.phase, p.round, p.attempt, p.body)
			return events.Draft{RunID: p.runID, Type: "publication.pending", Payload: map[string]any{"report_id": p.id, "phase": p.phase, "round": p.round, "attempt": p.attempt}}, err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// publishReports runs after phase reconciliation and never turns a posting error
// into a workflow error. It needs no agent, worktree, or active run to retry.
func (s *Scheduler) publishReports(ctx context.Context) error {
	if err := s.queueReports(ctx); err != nil {
		return err
	}
	if !s.cfg.PRComments {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT attempt_id,run_id,repository,pr_number,phase,round,attempt,body,warning_code FROM report_publications WHERE state='pending' ORDER BY COALESCE(last_attempt_at,''),run_id,round,CASE phase WHEN 'review' THEN 0 ELSE 1 END,attempt`)
	if err != nil {
		return err
	}
	var pending []pendingReport
	for rows.Next() {
		var p pendingReport
		if err := rows.Scan(&p.id, &p.runID, &p.repository, &p.pr, &p.phase, &p.round, &p.attempt, &p.body, &p.warning); err != nil {
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
	// Bound optional network work so unavailable GitHub cannot starve phases.
	publicationCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, p := range pending {
		if publicationCtx.Err() != nil {
			break
		}
		// Persist evidence of an attempted write before entering the adapter.
		if _, err := s.db.ExecContext(ctx, `UPDATE report_publications SET attempts=attempts+1,last_attempt_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE attempt_id=?`, p.id); err != nil {
			return err
		}
		var comment *github.Comment
		var cause error
		publisher, ok := s.deps.GitHub.(ReportPublisher)
		if !ok {
			cause = &fault.Error{Code: "publication.unsupported"}
		} else {
			comment, cause = publisher.EnsureReportComment(publicationCtx, p.repository, p.pr, p.body)
		}
		if cause == nil && (comment == nil || comment.ID <= 0 || comment.URL == "") {
			cause = &fault.Error{Code: "github.invalid_response"}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if cause != nil {
			code := publicationCode(cause)
			warning := "Report is saved locally. PR comment publication will retry without stopping work."
			if p.warning == code {
				continue
			}
			_, err = s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
				_, err := tx.ExecContext(ctx, `UPDATE report_publications SET warning_code=?,warning=? WHERE attempt_id=?`, code, warning, p.id)
				return events.Draft{RunID: p.runID, Type: "publication.warning", Payload: map[string]any{"report_id": p.id, "code": code, "message": warning}}, err
			})
		} else {
			_, err = s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
				_, err := tx.ExecContext(ctx, `UPDATE report_publications SET state='published',comment_id=?,comment_url=?,warning_code='',warning='' WHERE attempt_id=?`, comment.ID, comment.URL, p.id)
				return events.Draft{RunID: p.runID, Type: "publication.published", Payload: map[string]any{"report_id": p.id, "comment_id": comment.ID, "comment_url": comment.URL}}, err
			})
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Only known, fixed codes cross into events/UI; gh stderr may contain secrets.
func publicationCode(err error) string {
	var coded *fault.Error
	if errors.As(err, &coded) {
		switch coded.Code {
		case "github.unavailable", "github.rate_limited", "github.forbidden", "github.not_found", "github.not_logged_in", "github.canceled", "github.invalid_response", "github.invalid_input", "publication.multiple_matches", "publication.unsupported":
			return coded.Code
		}
	}
	return "publication.failed"
}
