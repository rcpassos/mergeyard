package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// Plan is a read-only snapshot of the claim queue. Empty queues encode as [].
type Plan struct {
	SchemaVersion int         `json:"schema_version"`
	Paused        bool        `json:"paused"`
	Issues        []PlanIssue `json:"issues"`
}

type PlanIssue struct {
	Repository   string                  `json:"repository"`
	Number       int                     `json:"number"`
	Title        string                  `json:"title"`
	Decision     string                  `json:"decision"`
	Reason       string                  `json:"reason,omitempty"`
	Blockers     []int                   `json:"blockers,omitempty"`
	RoleSettings *config.RunRoleSettings `json:"role_settings,omitempty"`
}

// Plan inspects current persisted state and GitHub without reconciliation,
// publication, workflow transitions, or agent/worktree operations.
func (s *Scheduler) Plan(ctx context.Context, repository string) (Plan, error) {
	s.tick.Lock()
	defer s.tick.Unlock()
	report := Plan{SchemaVersion: 1, Paused: s.control.Paused(), Issues: []PlanIssue{}}
	if repository != "" {
		if _, ok := s.repository(repository); !ok {
			return report, &fault.Error{Code: "plan.unknown_repository", Message: "Repository is not configured: " + repository}
		}
	}
	var runs []workflow.Run
	if s.db != nil {
		var err error
		runs, err = s.storedRuns(ctx)
		if err != nil {
			return report, fmt.Errorf("read persisted runs (start Mergeyard to migrate an older database): %w", err)
		}
	}
	capacity := capacityFor(runs, report.Paused)
	for _, repo := range s.cfg.Repositories {
		// Filtering is a projection of the whole queue, so earlier repositories
		// still reserve their would-claim slots against the global concurrency cap.
		issues, err := s.deps.GitHub.ListOpenIssues(ctx, repo.Repo)
		if err != nil {
			return report, err
		}
		for _, issue := range orderedIssues(issues) {
			if issue.State != github.Open || !hasLabel(issue, repo.Labels.Ready) {
				continue
			}
			candidate, err := s.inspectCandidate(ctx, repo, issue, capacity)
			if err != nil {
				return report, err
			}
			item := PlanIssue{Repository: repo.Repo, Number: issue.Number, Title: candidate.issue.Title, Decision: candidate.decision, Reason: candidate.reason, Blockers: candidate.blockers}
			if candidate.decision == "would_claim" {
				item.RoleSettings = candidate.settings
				capacity.reserve(repo.Repo, issue.Number)
			}
			if repository == "" || strings.EqualFold(repository, repo.Repo) {
				report.Issues = append(report.Issues, item)
			}
		}
	}
	return report, nil
}

// PlanOffline uses an existing read-only store, or nil for a workspace that has
// never been started. It constructs no writable runtime resources.
func PlanOffline(ctx context.Context, cfg config.Config, db *sql.DB, deps Dependencies, repository string) (Plan, error) {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	adapters := harness.Adapters(cfg.Agents)
	for name, adapter := range deps.Harnesses {
		adapters[name] = adapter
	}
	if deps.GitHub == nil {
		return Plan{}, fmt.Errorf("offline planning requires a GitHub reader")
	}
	s := &Scheduler{cfg: cfg, db: db, deps: deps, control: NewControl(nil), harnesses: adapters}
	if db != nil {
		s.workflow = workflow.New(db, nil)
	}
	return s.Plan(ctx, repository)
}

func (s *Scheduler) storedRuns(ctx context.Context) ([]workflow.Run, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM runs ORDER BY created_at, id")
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	runs := make([]workflow.Run, 0, len(ids))
	for _, id := range ids {
		run, err := s.workflow.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, nil
}
