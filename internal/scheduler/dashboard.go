package scheduler

import (
	"context"
	"sort"

	"github.com/rcpassos/mergeyard/internal/github"
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
