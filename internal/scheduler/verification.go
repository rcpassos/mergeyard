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

// Publish only the newest valid implement/fix report for each PR. Completion
// events are the durable intent; publication receipts prevent replay after restart.
// A new attempt supersedes an older failed delivery. Each retry reads the PR again.
// Attempts are recorded once per run in bounded bookkeeping rather than as an
// event row per tick. A finished run stops retrying only for failures retrying
// cannot resolve; transport errors, rate limits and budget expiry stay
// retryable, and an interrupted attempt stays retryable after restart.
func (s *Scheduler) publishVerification(ctx context.Context) error {
	// Evidence is materialized so the pass reads each completion event once, then
	// picks the newest report per run in a single pass. Re-deriving that report
	// per candidate row made the query quadratic in the event history.
	rows, err := s.db.QueryContext(ctx, `WITH evidence AS MATERIALIZED (
 SELECT e.id,e.run_id
 FROM events e WHERE e.type IN ('phase.completed','phase.failed')
 AND COALESCE(json_extract(e.payload_json,'$.phase'),'implement') IN ('implement','fix')
 AND json_type(e.payload_json,'$.result.verification')='array'
 AND (COALESCE(json_extract(e.payload_json,'$.error'),'')='' OR json_extract(e.payload_json,'$.error') LIKE 'phase.blocked:%' OR json_extract(e.payload_json,'$.error') LIKE 'phase.failed:%')
), newest AS (SELECT run_id,MAX(id) AS id FROM evidence GROUP BY run_id)
SELECT e.id,e.run_id,r.repository,r.pr_number,c.issue_json,json_extract(e.payload_json,'$.result'),COALESCE(r.role_settings_json,'null'),r.state,COALESCE(a.failed,0)
FROM newest n JOIN events e ON e.id=n.id JOIN runs r ON r.id=e.run_id JOIN scheduler_runs c ON c.run_id=r.id
LEFT JOIN verification_attempts a ON a.run_id=e.run_id AND a.source_event=e.id
WHERE r.pr_number>0
AND NOT EXISTS (SELECT 1 FROM events p WHERE p.run_id=e.run_id AND p.type='verification.published' AND json_extract(p.payload_json,'$.source_event')=e.id)
ORDER BY COALESCE(a.last_attempt_at,''),e.id`)
	if err != nil {
		return err
	}
	type publication struct {
		event       int64
		runID, repo string
		pr          int
		finished    bool
		content     github.PullRequestContent
	}
	var pending []publication
	for rows.Next() {
		var p publication
		var issueJSON, reportJSON, settingsJSON, runState string
		var failed bool
		if err := rows.Scan(&p.event, &p.runID, &p.repo, &p.pr, &issueJSON, &reportJSON, &settingsJSON, &runState, &failed); err != nil {
			rows.Close()
			return err
		}
		p.finished = workflow.State(runState).Terminal()
		// A finished run cannot deliver evidence to a reviewer. Once an attempt
		// has returned a failure it stops retrying; the warning stays visible.
		if p.finished && failed {
			continue
		}
		var issue github.Issue
		var report harness.PhaseResult
		if err := json.Unmarshal([]byte(issueJSON), &issue); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(reportJSON), &report); err != nil {
			rows.Close()
			return err
		}
		p.content = github.PullRequestContent{IssueNumber: issue.Number, IssueTitle: issue.Title, Summary: report.Summary, RunID: p.runID, Verification: report.Verification}
		if err := json.Unmarshal([]byte(settingsJSON), &p.content.RoleSettings); err != nil {
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
		if _, err := s.db.ExecContext(ctx, `INSERT INTO verification_attempts(run_id,source_event,attempts,last_attempt_at,failed)
			VALUES(?,?,1,strftime('%Y-%m-%dT%H:%M:%fZ','now'),0)
			ON CONFLICT(run_id) DO UPDATE SET
				attempts=CASE WHEN verification_attempts.source_event=excluded.source_event THEN verification_attempts.attempts+1 ELSE 1 END,
				source_event=excluded.source_event,
				last_attempt_at=excluded.last_attempt_at,
				failed=0`, p.runID, p.event); err != nil {
			return err
		}
		_, cause := s.deps.GitHub.UpdatePullRequest(publicationCtx, p.repo, p.pr, p.content)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if cause != nil {
			code := publicationCode(cause)
			persistent := persistentPublicationFailure(cause, publicationCtx)
			message := "Verification evidence is saved locally. PR body publication will retry."
			if persistent {
				message = "Verification evidence is saved locally. PR body publication cannot update this PR."
			}
			// Keep publication failures visible without repeating the same warning on every tick.
			var exists bool
			if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE run_id=? AND type='verification.warning' AND json_extract(payload_json,'$.source_event')=? AND json_extract(payload_json,'$.code')=?)`, p.runID, p.event, code).Scan(&exists); err != nil {
				return err
			}
			// A finished run stops on its next tick only when retrying cannot
			// resolve the failure. The stop marker commits with a new warning so
			// neither survives without the other.
			stop := p.finished && persistent
			if !exists {
				warning := events.Draft{RunID: p.runID, Type: "verification.warning", Payload: map[string]any{"source_event": p.event, "code": code, "message": message}}
				if _, err := s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
					if stop {
						if _, err := tx.ExecContext(ctx, `UPDATE verification_attempts SET failed=1 WHERE run_id=? AND source_event=?`, p.runID, p.event); err != nil {
							return events.Draft{}, err
						}
					}
					return warning, nil
				}); err != nil {
					return err
				}
			} else if stop {
				if _, err := s.db.ExecContext(ctx, `UPDATE verification_attempts SET failed=1 WHERE run_id=? AND source_event=?`, p.runID, p.event); err != nil {
					return err
				}
			}
			continue
		}
		if _, err := s.bus.Publish(ctx, events.Draft{RunID: p.runID, Type: "verification.published", Payload: map[string]any{"source_event": p.event, "pr_number": p.pr}}); err != nil {
			return err
		}
	}
	return nil
}
