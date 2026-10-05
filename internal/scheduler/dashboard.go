package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"sort"

	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// QueueIssue is an open ready, blocked, or attention issue discovered on GitHub.
type QueueIssue struct {
	Repository string
	Issue      github.Issue
	Blockers   []github.Issue
	Attention  bool
}

// Queue discovers issues without claiming them, even when paused or at capacity.
// A dependency failure is returned rather than misrepresenting an issue as ready.
func (s *Scheduler) Queue(ctx context.Context) ([]QueueIssue, error) {
	var queue []QueueIssue
	for _, repo := range s.cfg.Repositories {
		if !repo.Enabled {
			continue
		}
		issues, err := s.deps.GitHub.ListOpenIssues(ctx, repo.Repo)
		if err != nil {
			return nil, err
		}
		issues = append([]github.Issue(nil), issues...)
		sort.Slice(issues, func(i, j int) bool {
			if issues[i].CreatedAt.Equal(issues[j].CreatedAt) {
				return issues[i].Number < issues[j].Number
			}
			return issues[i].CreatedAt.Before(issues[j].CreatedAt)
		})
		for _, issue := range issues {
			if issue.State != github.Open {
				continue
			}
			attention := hasLabel(issue, repo.Labels.NeedsAttention)
			if !attention && (!hasLabel(issue, repo.Labels.Ready) || hasLabel(issue, repo.Labels.Running)) {
				continue
			}
			item := QueueIssue{Repository: repo.Repo, Issue: issue, Attention: attention}
			if !attention {
				item.Blockers, err = s.deps.GitHub.UnresolvedBlockers(ctx, repo.Repo, issue.Number)
				if err != nil {
					return nil, err
				}
			}
			queue = append(queue, item)
		}
	}
	return queue, nil
}

// Stop is the shared lifecycle control for browser and CLI callers. It waits
// for any in-flight operation, stops the phase, and preserves Git artifacts.
// Repeating Stop retries label cleanup without restarting or re-transitioning.
func (s *Scheduler) Stop(ctx context.Context, id string) error {
	return s.workflow.WithRunOperation(ctx, id, func(ctx context.Context, run workflow.Run) error {
		return s.stopRun(ctx, run)
	})
}

func (s *Scheduler) stopRun(ctx context.Context, run workflow.Run) error {
	if run.State.Terminal() && run.State != workflow.Stopped {
		return &fault.Error{Code: "internal.transition_invalid", Message: "This run has already ended"}
	}
	repo, ok := s.repository(run.Repository)
	if !ok {
		return &fault.Error{Code: "config.repository_missing", Message: "Run repository is missing from the effective configuration"}
	}
	if _, err := s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		_, err := tx.ExecContext(ctx, "UPDATE runs SET stop_requested=1 WHERE id=?", run.ID)
		return events.Draft{RunID: run.ID, Type: "run.stop_requested", Payload: map[string]bool{"stop_requested": true}}, err
	}); err != nil {
		return err
	}
	if run.State != workflow.Stopped {
		a, err := s.lastAttempt(ctx, run.ID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && a.status == "running" {
			if err := s.deps.Runner.StopSession(ctx, a.ref); err != nil {
				return err
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE phase_attempts SET status='stopped',ended_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, a.id); err != nil {
				return err
			}
		}
	}
	if err := s.deps.GitHub.RemoveLabel(ctx, repo.Repo, run.IssueNumber, repo.Labels.Running); err != nil {
		return err
	}
	var path, branch string
	var pr int
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE(worktree_path,''),COALESCE(branch,''),COALESCE(pr_number,0) FROM runs WHERE id=?", run.ID).Scan(&path, &branch, &pr); err != nil {
		return err
	}
	if path != "" || branch != "" || pr > 0 {
		if err := s.deps.GitHub.AddLabel(ctx, repo.Repo, run.IssueNumber, repo.Labels.NeedsAttention); err != nil {
			return err
		}
	}
	if run.State != workflow.Stopped {
		_, err := s.workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.Stop})
		return err
	}
	return nil
}
