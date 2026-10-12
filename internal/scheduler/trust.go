package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
)

func (s *Scheduler) authorizeIssue(ctx context.Context, repo config.Repository, issue github.Issue) (bool, error) {
	decision, err := s.deps.GitHub.VerifyIssueTrust(ctx, repo.Repo, issue, repo.Labels.Ready, repo.TrustedLabelers)
	return s.applyTrustDecision(ctx, repo, issue, decision, err)
}

func (s *Scheduler) recheckIssue(ctx context.Context, repo config.Repository, issue github.Issue) (bool, error) {
	decision, err := s.deps.GitHub.RecheckIssueTrust(ctx, repo.Repo, issue, repo.Labels.Ready, repo.TrustedLabelers)
	return s.applyTrustDecision(ctx, repo, issue, decision, err)
}

func (s *Scheduler) recoverIssueSnapshot(ctx context.Context, repo config.Repository, runID string, issue github.Issue) error {
	allowed, err := s.recheckIssue(ctx, repo, issue)
	if err != nil {
		return err
	}
	if !allowed {
		return &fault.Error{Code: "github.issue_untrusted", Message: "Cannot recover claim without an authorized issue snapshot"}
	}
	return s.saveIssue(ctx, runID, issue)
}

func (s *Scheduler) applyTrustDecision(ctx context.Context, repo config.Repository, issue github.Issue, decision github.TrustDecision, err error) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil {
		code := "github.trust_unverifiable"
		var coded *fault.Error
		if errors.As(err, &coded) {
			code = coded.Code
		}
		decision = github.TrustDecision{Reason: fmt.Sprintf("Cannot verify label actors or issue body edit history (%s).", code)}
	}
	if decision.Allowed {
		_, err = s.db.ExecContext(ctx, "DELETE FROM issue_refusals WHERE repository=? AND issue_number=?", strings.ToLower(repo.Repo), issue.Number)
		return err == nil, err
	}
	if decision.Reason == "" {
		decision.Reason = "Cannot verify authorization for this issue."
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO issue_refusals(repository,issue_number,reason,ready_label,pending) VALUES(?,?,?,?,1)
 ON CONFLICT(repository,issue_number) DO UPDATE SET reason=excluded.reason,ready_label=excluded.ready_label,pending=1`, strings.ToLower(repo.Repo), issue.Number, decision.Reason, repo.Labels.Ready)
	if err != nil {
		return false, err
	}
	return false, s.finishRefusal(ctx, repo.Repo, issue.Number, repo.Labels.Ready, decision.Reason)
}

func (s *Scheduler) finishRefusal(ctx context.Context, repo string, number int, ready, reason string) error {
	// Comment first: if removal succeeds but the daemon dies, the explanation
	// already exists. Ambiguous writes replay via a marker lookup, never POST alone.
	if err := s.deps.GitHub.EnsureTrustRefusalComment(ctx, repo, number, reason); err != nil {
		return err
	}
	if err := s.deps.GitHub.RemoveLabel(ctx, repo, number, ready); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, "UPDATE issue_refusals SET pending=0 WHERE repository=? AND issue_number=?", strings.ToLower(repo), number)
	return err
}

func (s *Scheduler) reconcileRefusals(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT repository,issue_number,ready_label,reason FROM issue_refusals WHERE pending=1 ORDER BY repository,issue_number")
	if err != nil {
		return err
	}
	type pendingRefusal struct {
		repo          string
		number        int
		ready, reason string
	}
	var pending []pendingRefusal
	for rows.Next() {
		var refusal pendingRefusal
		if err := rows.Scan(&refusal.repo, &refusal.number, &refusal.ready, &refusal.reason); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, refusal)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var failures []error
	for _, refusal := range pending {
		repo, ok := s.repository(refusal.repo)
		if !ok || !repo.Enabled {
			continue
		}
		issue, err := s.deps.GitHub.GetIssue(ctx, repo.Repo, refusal.number)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if hasLabel(issue, refusal.ready) {
			// A maintainer may have reapplied authorization while removal was
			// pending. Recheck without the poll cache before mutating that label.
			// Use its saved name even if configuration has since renamed ready.
			repo.Labels.Ready = refusal.ready
			if _, err := s.recheckIssue(ctx, repo, issue); err != nil {
				failures = append(failures, err)
			}
			continue
		}
		if err := s.finishRefusal(ctx, repo.Repo, refusal.number, refusal.ready, refusal.reason); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (s *Scheduler) refusalReason(ctx context.Context, repo string, number int) (string, error) {
	var reason string
	err := s.db.QueryRowContext(ctx, "SELECT reason FROM issue_refusals WHERE repository=? AND issue_number=?", strings.ToLower(repo), number).Scan(&reason)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return reason, err
}
