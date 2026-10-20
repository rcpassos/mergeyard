package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// Publish only the newest valid implement/fix report for each PR. The report
// saved on the attempt is the durable intent; publication receipts prevent
// replay after restart. A new attempt supersedes an older failed delivery. Each
// retry reads the PR again. Attempts are recorded once per run in bounded
// bookkeeping rather than as an event row per tick. Permanent answers stop
// retries, and a finished run stops after a returned failure. Transient
// failures back off; a shared-budget expiry and an interrupted attempt stay
// retryable after restart.
func (s *Scheduler) publishVerification(ctx context.Context) error {
	// Reports are read from the attempts that own them, never re-derived from the
	// event history. Eligibility is frozen on the attempt when its report is
	// saved, so a later lifecycle rejection never withdraws it. The newest
	// publishable attempt per run wins; receipts and attempt order stay per run
	// so one slow PR cannot monopolize retries.
	rows, err := s.db.QueryContext(ctx, `WITH evidence AS MATERIALIZED (
 SELECT a.rowid AS seq,a.id,a.run_id,a.report_json AS report
 FROM phase_attempts a
 WHERE a.phase='implement' AND a.step='' AND a.report_eligible=1
 UNION ALL
 SELECT a.rowid,a.id,a.run_id,f.report_json
 FROM fix_attempts f JOIN phase_attempts a ON a.id=f.attempt_id
 WHERE f.report_json IS NOT NULL AND a.report_eligible=1
), publishable AS (
 SELECT run_id,seq,id,report FROM evidence
 WHERE json_type(report,'$.verification')='array'
), newest AS (SELECT run_id,MAX(seq) AS seq FROM publishable GROUP BY run_id)
SELECT e.id,e.run_id,r.repository,r.pr_number,c.issue_json,e.report,COALESCE(r.role_settings_json,'null'),r.state,COALESCE(a.failed,0),COALESCE(a.stopped,0),COALESCE(a.attempts,0),COALESCE(a.last_attempt_at,'')
FROM newest n JOIN publishable e ON e.run_id=n.run_id AND e.seq=n.seq
JOIN runs r ON r.id=e.run_id JOIN scheduler_runs c ON c.run_id=r.id
LEFT JOIN verification_attempts a ON a.run_id=e.run_id AND a.source_attempt=e.id
WHERE r.pr_number>0
AND NOT EXISTS (SELECT 1 FROM events p WHERE p.run_id=e.run_id AND p.type='verification.published' AND json_extract(p.payload_json,'$.source_attempt')=e.id)
ORDER BY COALESCE(a.last_attempt_at,''),e.seq`)
	if err != nil {
		return err
	}
	type publication struct {
		source       string
		runID, repo  string
		pr, attempts int
		lastAttempt  string
		finished     bool
		content      github.PullRequestContent
	}
	type loadedPublication struct {
		publication
		issueJSON, reportJSON, settingsJSON string
		failed, stopped                     bool
	}
	var loaded []loadedPublication
	for rows.Next() {
		var p loadedPublication
		var runState string
		if err := rows.Scan(&p.source, &p.runID, &p.repo, &p.pr, &p.issueJSON, &p.reportJSON, &p.settingsJSON, &runState, &p.failed, &p.stopped, &p.attempts, &p.lastAttempt); err != nil {
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
	var pending []publication
	for _, p := range loaded {
		state := publicationState{Attempts: p.attempts, LastAttemptAt: p.lastAttempt, Finished: p.finished, Failed: p.failed, Stopped: p.stopped}
		disposition, err := state.disposition(s.deps.Now())
		if err != nil {
			return err
		}
		switch disposition {
		case publicationStop:
			message := stoppedPublicationWarning("Verification evidence is saved locally. This run is finished, so PR body publication will not retry.")
			payload := map[string]any{"source_attempt": p.source, "message": message}
			if err := s.stopPublication(ctx, p.runID, payload, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `UPDATE verification_attempts SET stopped=1 WHERE run_id=? AND source_attempt=?`, p.runID, p.source)
				return err
			}); err != nil {
				return err
			}
		case publicationReady:
			var issue github.Issue
			var report harness.PhaseResult
			if err := json.Unmarshal([]byte(p.issueJSON), &issue); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(p.reportJSON), &report); err != nil {
				return err
			}
			p.content = github.PullRequestContent{IssueNumber: issue.Number, IssueTitle: issue.Title, Summary: report.Summary, RunID: p.runID, Verification: report.Verification}
			if err := json.Unmarshal([]byte(p.settingsJSON), &p.content.RoleSettings); err != nil {
				return err
			}
			pending = append(pending, p.publication)
		}
	}
	// Share a short budget across the batch so optional GitHub writes cannot
	// starve reconciliation. Persist warnings and receipts with the caller context.
	publicationCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, p := range pending {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if publicationCtx.Err() != nil {
			break
		}
		// Record the attempt before transport so a slow report rotates behind
		// untouched reports even if the scheduler restarts or the call never
		// returns. One row per run replaces the old event row per attempt.
		attemptedAt := publicationTimestamp(s.deps.Now())
		if _, err := s.db.ExecContext(ctx, `INSERT INTO verification_attempts(run_id,source_attempt,attempts,last_attempt_at,failed,stopped)
			VALUES(?,?,1,?,0,0)
			ON CONFLICT(run_id) DO UPDATE SET
				attempts=CASE WHEN verification_attempts.source_attempt=excluded.source_attempt THEN verification_attempts.attempts+1 ELSE 1 END,
				source_attempt=excluded.source_attempt,
				last_attempt_at=excluded.last_attempt_at,
				failed=0,
				stopped=0`, p.runID, p.source, attemptedAt); err != nil {
			return err
		}
		_, cause := s.deps.GitHub.UpdatePullRequest(publicationCtx, p.repo, p.pr, p.content)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if cause != nil {
			outcome := outcomeFor(cause, publicationCtx, p.finished, publicationWarnings{
				Deferred: "Verification evidence is saved locally. PR body publication is deferred.",
				Terminal: stoppedPublicationWarning("Verification evidence is saved locally. PR body publication cannot update this PR."),
				Finished: stoppedPublicationWarning("Verification evidence is saved locally. This run is finished, so PR body publication will not retry."),
			})
			// Keep publication failures visible without repeating the same warning on every tick.
			var exists bool
			if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE run_id=? AND type='verification.warning' AND json_extract(payload_json,'$.source_attempt')=? AND json_extract(payload_json,'$.code')=?)`, p.runID, p.source, outcome.Code).Scan(&exists); err != nil {
				return err
			}
			// A new warning and its result commit together. An existing warning
			// stays append-only; a stop appends a visible stop event instead.
			if !exists {
				warning := events.Draft{RunID: p.runID, Type: "verification.warning", Payload: map[string]any{"source_attempt": p.source, "code": outcome.Code, "message": outcome.Message}}
				if _, err := s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
					if _, err := tx.ExecContext(ctx, `UPDATE verification_attempts SET failed=?,stopped=? WHERE run_id=? AND source_attempt=?`, outcome.Failed, outcome.Stopped, p.runID, p.source); err != nil {
						return events.Draft{}, err
					}
					return warning, nil
				}); err != nil {
					return err
				}
			} else if outcome.Stopped {
				payload := map[string]any{"source_attempt": p.source, "code": outcome.Code, "message": outcome.Message}
				if err := s.stopPublication(ctx, p.runID, payload, func(tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, `UPDATE verification_attempts SET failed=?,stopped=1 WHERE run_id=? AND source_attempt=?`, outcome.Failed, p.runID, p.source)
					return err
				}); err != nil {
					return err
				}
			} else {
				if _, err := s.db.ExecContext(ctx, `UPDATE verification_attempts SET failed=?,stopped=? WHERE run_id=? AND source_attempt=?`, outcome.Failed, outcome.Stopped, p.runID, p.source); err != nil {
					return err
				}
			}
			continue
		}
		if _, err := s.bus.Publish(ctx, events.Draft{RunID: p.runID, Type: "verification.published", Payload: map[string]any{"source_attempt": p.source, "pr_number": p.pr}}); err != nil {
			return err
		}
	}
	return nil
}
