package scheduler

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/harness"
)

// Publish only the newest valid implement/fix report for each PR. Completion
// events are the durable intent; publication receipts prevent replay after restart.
// A new attempt supersedes an older failed delivery. Each retry reads the PR again.
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
SELECT e.id,e.run_id,r.repository,r.pr_number,c.issue_json,json_extract(e.payload_json,'$.result'),COALESCE(r.role_settings_json,'null')
FROM newest n JOIN events e ON e.id=n.id JOIN runs r ON r.id=e.run_id JOIN scheduler_runs c ON c.run_id=r.id
WHERE r.pr_number>0
AND NOT EXISTS (SELECT 1 FROM events p WHERE p.run_id=e.run_id AND p.type='verification.published' AND json_extract(p.payload_json,'$.source_event')=e.id)
ORDER BY COALESCE((SELECT MAX(p.id) FROM events p
 WHERE p.run_id=e.run_id AND p.type='verification.attempted'
 AND json_extract(p.payload_json,'$.source_event')=e.id),0),e.id`)
	if err != nil {
		return err
	}
	type publication struct {
		event       int64
		runID, repo string
		pr          int
		content     github.PullRequestContent
	}
	var pending []publication
	for rows.Next() {
		var p publication
		var issueJSON, reportJSON, settingsJSON string
		if err := rows.Scan(&p.event, &p.runID, &p.repo, &p.pr, &issueJSON, &reportJSON, &settingsJSON); err != nil {
			rows.Close()
			return err
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
		// Journal attempts before transport so a slow report rotates behind
		// untouched reports even if the scheduler restarts or the write is ambiguous.
		if _, err := s.bus.Publish(ctx, events.Draft{RunID: p.runID, Type: "verification.attempted", Payload: map[string]any{"source_event": p.event, "pr_number": p.pr}}); err != nil {
			return err
		}
		_, cause := s.deps.GitHub.UpdatePullRequest(publicationCtx, p.repo, p.pr, p.content)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if cause != nil {
			code := publicationCode(cause)
			// Keep publication failures visible without repeating the same warning on every tick.
			var exists bool
			if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE run_id=? AND type='verification.warning' AND json_extract(payload_json,'$.source_event')=? AND json_extract(payload_json,'$.code')=?)`, p.runID, p.event, code).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				if _, err := s.bus.Publish(ctx, events.Draft{RunID: p.runID, Type: "verification.warning", Payload: map[string]any{"source_event": p.event, "code": code, "message": "Verification evidence is saved locally. PR body publication will retry."}}); err != nil {
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
