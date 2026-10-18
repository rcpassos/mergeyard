package scheduler

import (
	"context"
	"sort"
	"strings"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// claimCapacity models the current queue and the slots reserved earlier in a poll.
// Planning reserves only these in-memory slots; dispatch reserves persisted runs.
type claimCapacity struct {
	slots        int
	repositories map[string]int
	existing     map[string]bool
	paused       bool
}

func capacityFor(runs []workflow.Run, paused bool) claimCapacity {
	c := claimCapacity{repositories: map[string]int{}, existing: map[string]bool{}, paused: paused}
	for _, run := range runs {
		if run.Merge != nil && run.Merge.Pending() {
			c.existing[issueKey(run.Repository, run.IssueNumber)] = true
		}
		if !run.State.Terminal() {
			c.existing[issueKey(run.Repository, run.IssueNumber)] = true
			if run.State != workflow.ReadyToMerge {
				c.slots++
				c.repositories[strings.ToLower(run.Repository)]++
			}
		}
	}
	return c
}

func (c *claimCapacity) reserve(repo string, number int) {
	c.slots++
	c.repositories[strings.ToLower(repo)]++
	c.existing[issueKey(repo, number)] = true
}

func orderedIssues(issues []github.Issue) []github.Issue {
	issues = append([]github.Issue(nil), issues...)
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].CreatedAt.Equal(issues[j].CreatedAt) {
			return issues[i].Number < issues[j].Number
		}
		return issues[i].CreatedAt.Before(issues[j].CreatedAt)
	})
	return issues
}

// eligibility is pure: both plan and dispatch use this policy, supplying observed
// facts in stages so GitHub is queried only for candidates that can use a slot.
func eligibility(repo config.Repository, issue github.Issue, c claimCapacity, globalLimit int) (string, string) {
	switch {
	case issue.State != github.Open || !hasLabel(issue, repo.Labels.Ready):
		return "skipped", "not_ready"
	case !repo.Enabled:
		return "skipped", "repo_disabled"
	case hasLabel(issue, repo.Labels.Running):
		return "skipped", "running_label"
	case hasLabel(issue, repo.Labels.NeedsAttention):
		return "skipped", "needs_attention_label"
	case c.existing[issueKey(repo.Repo, issue.Number)]:
		return "skipped", "active_run"
	case c.paused:
		return "waiting", "scheduler_paused"
	}
	limit := repo.Concurrency
	if limit <= 0 {
		limit = globalLimit
	}
	if c.slots >= globalLimit || c.repositories[strings.ToLower(repo.Repo)] >= limit {
		return "waiting", "concurrency_full"
	}
	return "would_claim", ""
}

type candidate struct {
	issue            github.Issue
	decision, reason string
	blockers         []int
	trust            github.TrustDecision
	trustErr         error
	settings         *config.RunRoleSettings
	settingsErr      error
}

// inspectCandidate is the shared read-only eligibility path. Authorization and
// invalid-override publication are deliberately left to the dispatch caller.
func (s *Scheduler) inspectCandidate(ctx context.Context, repo config.Repository, issue github.Issue, c claimCapacity) (candidate, error) {
	v := candidate{issue: issue}
	v.decision, v.reason = eligibility(repo, issue, c, s.cfg.Concurrency)
	if v.decision != "would_claim" {
		return v, nil
	}
	limited, err := s.gateLimitedIssue(ctx, repo, issue)
	if err != nil {
		return v, err
	}
	if limited {
		return s.inspectLimitedCandidate(ctx, repo, v)
	}
	v.trust, v.trustErr = s.deps.GitHub.VerifyIssueTrust(ctx, repo.Repo, issue, repo.Labels.Ready, repo.TrustedLabelers)
	if ctx.Err() != nil {
		return v, ctx.Err()
	}
	if v.trustErr != nil || !v.trust.Allowed {
		v.decision, v.reason = "skipped", "untrusted_labeler"
		return v, nil
	}
	blockers, err := s.deps.GitHub.UnresolvedBlockers(ctx, repo.Repo, issue.Number)
	if err != nil {
		return v, err
	}
	if len(blockers) > 0 {
		for _, blocker := range blockers {
			v.blockers = append(v.blockers, blocker.Number)
		}
		sort.Ints(v.blockers)
		v.decision, v.reason = "skipped", "blocked_by"
		return v, nil
	}
	// Freeze the same fresh snapshot used immediately before a real claim.
	v.issue, err = s.deps.GitHub.GetIssue(ctx, repo.Repo, issue.Number)
	if err != nil {
		return v, err
	}
	v.decision, v.reason = eligibility(repo, v.issue, c, s.cfg.Concurrency)
	if v.decision != "would_claim" {
		return v, nil
	}
	v.trust, v.trustErr = s.deps.GitHub.RecheckIssueTrust(ctx, repo.Repo, v.issue, repo.Labels.Ready, repo.TrustedLabelers)
	if ctx.Err() != nil {
		return v, ctx.Err()
	}
	if v.trustErr != nil || !v.trust.Allowed {
		v.decision, v.reason = "skipped", "untrusted_labeler"
		return v, nil
	}
	v.settings, v.settingsErr = s.resolveIssueRoleSettings(repo, v.issue)
	if v.settingsErr != nil {
		v.decision, v.reason = "skipped", "invalid_harness_override"
		return v, nil
	}
	// Rechecked on the snapshot being claimed: its labels can override to a
	// harness the listing did not name.
	limited, err = s.harnessLimited(ctx, v.settings.Implementer.Agent)
	if err != nil {
		return v, err
	}
	if limited {
		v.decision, v.reason = "waiting", "harness_limited"
	}
	return v, nil
}

// inspectLimitedCandidate withholds a candidate the listing reports as limited.
// A previously refused issue whose ready label is back still has its cached trust
// read, so dispatch can clear or republish the refusal instead of the dashboard
// showing a stale refusal for the whole limit.
func (s *Scheduler) inspectLimitedCandidate(ctx context.Context, repo config.Repository, v candidate) (candidate, error) {
	v.decision, v.reason = "waiting", "harness_limited"
	refusal, err := s.refusalReason(ctx, repo.Repo, v.issue.Number)
	if err != nil || refusal == "" {
		return v, err
	}
	v.trust, v.trustErr = s.deps.GitHub.VerifyIssueTrust(ctx, repo.Repo, v.issue, repo.Labels.Ready, repo.TrustedLabelers)
	if ctx.Err() != nil {
		return v, ctx.Err()
	}
	if v.trustErr != nil || !v.trust.Allowed {
		v.decision, v.reason = "skipped", "untrusted_labeler"
	}
	return v, nil
}
