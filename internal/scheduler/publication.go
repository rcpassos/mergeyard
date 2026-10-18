package scheduler

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/rcpassos/mergeyard/internal/contract"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/review"
)

// ReportPublisher performs inspect-before-create, including after ambiguous writes.
type ReportPublisher interface {
	EnsureReportComment(context.Context, string, int, string) (*github.Comment, error)
}

type pendingReport struct {
	id, runID, repository, phase, body, warning, checkRetryOf string
	pr, round, attempt                                        int
}

// queueReports reconstructs missing publication intents from locally saved
// reports, including terminal runs. Frozen bodies are never regenerated on replay.
func (s *Scheduler) queueReports(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT a.id,a.run_id,r.repository,r.pr_number,a.phase,a.round,a.attempt,COALESCE(v.report_json,f.report_json),COALESCE(v.accepted,0),a.check_retry_of
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
		if err := rows.Scan(&p.id, &p.runID, &p.repository, &p.pr, &p.phase, &p.round, &p.attempt, &raw, &accepted, &p.checkRetryOf); err != nil {
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
			RunID        string `json:"run_id"`
			AttemptID    string `json:"report_id"`
			Accepted     *bool  `json:"accepted,omitempty"`
			CheckRetryOf string `json:"check_retry_of,omitempty"`
			Report       any    `json:"report"`
		}{p.runID, p.id, acceptedReview, p.checkRetryOf, report}, "", "  ")
		if err != nil {
			rows.Close()
			return err
		}
		title := "Review"
		outcomes := ""
		if p.phase == "review" {
			var r review.Report
			if err := json.Unmarshal([]byte(raw), &r); err != nil {
				rows.Close()
				return err
			}
			if requirement, ok := contract.For(contract.Review, r.SchemaVersion); ok && requirement.Requires(contract.Criteria) {
				outcomes = "\n\nAcceptance criteria:"
				if len(r.Criteria) == 0 {
					outcomes += " no acceptance criteria found."
				}
				for _, c := range r.Criteria {
					outcomes += " " + c.ID + " " + c.Verdict + ";"
				}
			}
			counts := r.PriorCounts()
			outcomes += fmt.Sprintf("\n\nPrior findings: %d fixed / %d not fixed / %d no longer applies.", counts.Fixed, counts.NotFixed, counts.NoLongerApplies)
		}
		if p.phase == "fix" {
			title = "Fix"
		}
		annotation := ""
		if p.phase == "fix" && p.checkRetryOf != "" {
			annotation = "\n\nCheck retry after failed local checks."
		}
		identity := sha256.Sum256([]byte(p.runID + "\x00" + p.phase + "\x00" + p.id))
		p.body = fmt.Sprintf("<!-- mergeyard:report:%x -->\n## %s report · round %d, attempt %d%s\n\n```json\n%s\n```", identity, title, p.round, p.attempt, annotation+outcomes, strings.ReplaceAll(string(encoded), "`", `\u0060`))
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
	deliveries := make([]commentDelivery, 0, len(pending))
	for _, p := range pending {
		deliveries = append(deliveries, p.delivery(s))
	}
	return s.publishComments(ctx, deliveries)
}

func (p pendingReport) delivery(s *Scheduler) commentDelivery {
	warning := "Report is saved locally. PR comment publication will retry without stopping work."
	return commentDelivery{
		repository: p.repository, number: p.pr, body: p.body, warningCode: p.warning,
		startAttempt: func(ctx context.Context) error {
			_, err := s.db.ExecContext(ctx, `UPDATE report_publications SET attempts=attempts+1,last_attempt_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE attempt_id=?`, p.id)
			return err
		},
		saveWarning: func(ctx context.Context, tx *sql.Tx, code string) (events.Draft, error) {
			_, err := tx.ExecContext(ctx, `UPDATE report_publications SET warning_code=?,warning=? WHERE attempt_id=?`, code, warning, p.id)
			return events.Draft{RunID: p.runID, Type: "publication.warning", Payload: map[string]any{"report_id": p.id, "code": code, "message": warning}}, err
		},
		saveReceipt: func(ctx context.Context, tx *sql.Tx, comment *github.Comment) (events.Draft, error) {
			_, err := tx.ExecContext(ctx, `UPDATE report_publications SET state='published',comment_id=?,comment_url=?,warning_code='',warning='' WHERE attempt_id=?`, comment.ID, comment.URL, p.id)
			return events.Draft{RunID: p.runID, Type: "publication.published", Payload: map[string]any{"report_id": p.id, "comment_id": comment.ID, "comment_url": comment.URL}}, err
		},
	}
}

// persistentPublicationFailure reports whether retrying cannot resolve a
// returned body-publication failure: the PR is closed or inaccessible, or this
// runtime cannot write PR bodies at all. Transport errors, rate limits, invalid
// responses and a shared-budget expiry stay retryable.
func persistentPublicationFailure(cause error, publicationCtx context.Context) bool {
	var coded *fault.Error
	if publicationCtx.Err() != nil || !errors.As(cause, &coded) {
		return false
	}
	switch coded.Code {
	case "github.not_found", "github.forbidden", "github.invalid_input", "publication.unsupported", "publication.multiple_matches":
		return true
	}
	return false
}

// Only known, fixed codes cross into events/UI; gh stderr may contain secrets.
func publicationCode(err error) string {
	var coded *fault.Error
	if errors.As(err, &coded) {
		switch coded.Code {
		case "github.unavailable", "github.rate_limited", "github.forbidden", "github.not_found", "github.not_logged_in", "github.canceled", "github.timeout", "github.invalid_response", "github.invalid_input", "publication.multiple_matches", "publication.unsupported":
			return coded.Code
		}
	}
	return "publication.failed"
}
