package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/github"
)

func (s *Scheduler) resolveIssueRoleSettings(repo config.Repository, issue github.Issue) (*config.RunRoleSettings, error) {
	labels := make([]string, len(issue.Labels))
	for i, label := range issue.Labels {
		labels[i] = label.Name
	}

	settings, err := config.ResolveRoleSettings(repo, labels)
	if err != nil {
		return nil, err
	}
	resolved := settings.Apply(repo)
	var failures []error
	for _, selection := range []struct {
		name string
		role config.Role
	}{{"implementer", resolved.Implementer}, {"reviewer", resolved.Reviewer}} {
		if err := validateHarnessRole(s.harnesses, selection.role); err != nil {
			failures = append(failures, fmt.Errorf("Resolved %s settings for %s are incompatible: %w. Correct the override labels or role configuration, then remove the needs-attention label", selection.name, selection.role.Agent, err))
		}
	}
	if err := errors.Join(failures...); err != nil {
		return nil, err
	}
	return settings, nil
}

func (s *Scheduler) refuseHarnessOverride(ctx context.Context, repo config.Repository, number int, reason string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO harness_override_refusals(repository,issue_number,reason,attention_label,pending) VALUES(?,?,?,?,1)
 ON CONFLICT(repository,issue_number) DO UPDATE SET reason=excluded.reason,attention_label=excluded.attention_label,pending=1`, strings.ToLower(repo.Repo), number, reason, repo.Labels.NeedsAttention)
	if err != nil {
		return err
	}
	return s.finishHarnessOverrideRefusal(ctx, repo.Repo, number, repo.Labels.NeedsAttention, reason)
}

func (s *Scheduler) finishHarnessOverrideRefusal(ctx context.Context, repo string, number int, attention, reason string) error {
	// The marker lookup makes replay safe after an ambiguous comment write.
	if err := s.deps.GitHub.EnsureHarnessOverrideComment(ctx, repo, number, reason); err != nil {
		return err
	}
	if err := s.deps.GitHub.AddLabel(ctx, repo, number, attention); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, "UPDATE harness_override_refusals SET pending=0 WHERE repository=? AND issue_number=?", strings.ToLower(repo), number)
	return err
}

func (s *Scheduler) reconcileHarnessOverrideRefusals(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT repository,issue_number,attention_label,reason FROM harness_override_refusals WHERE pending=1 ORDER BY repository,issue_number")
	if err != nil {
		return err
	}
	type refusal struct {
		repo, attention, reason string
		number                  int
	}
	var pending []refusal
	for rows.Next() {
		var v refusal
		if err := rows.Scan(&v.repo, &v.number, &v.attention, &v.reason); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var failures []error
	for _, v := range pending {
		repo, ok := s.repository(v.repo)
		if !ok || !repo.Enabled {
			continue
		}
		issue, err := s.deps.GitHub.GetIssue(ctx, repo.Repo, v.number)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		_, overrideErr := s.resolveIssueRoleSettings(repo, issue)
		if issue.State != github.Open || overrideErr == nil {
			// An attention label may have been applied despite a lost response.
			// Preserve a maintainer's correction instead of replaying stale intent.
			if _, err := s.db.ExecContext(ctx, "UPDATE harness_override_refusals SET pending=0 WHERE repository=? AND issue_number=?", v.repo, v.number); err != nil {
				failures = append(failures, err)
			}
			continue
		}
		if err := s.finishHarnessOverrideRefusal(ctx, repo.Repo, v.number, v.attention, v.reason); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
