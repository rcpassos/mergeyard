package scheduler

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
)

// QueueIssue is an open ready, blocked, or attention issue discovered on GitHub.
type QueueIssue struct {
	Repository    string
	Issue         github.Issue
	Blockers      []github.Issue
	Attention     bool
	RefusalReason string
}

// blockerEntry is the open blockers shown for one queue issue, keyed by the
// listing snapshot and open-blocker count they were read for. Only an
// unchanged snapshot makes another dependency read safe to skip; a changed
// issue or count refreshes it. A same-count replacement of a blocker stays
// cached until the issue changes, because the listing exposes no finer signal.
type blockerEntry struct {
	updatedAt time.Time
	blockedBy int
	blockers  []github.Issue
}

// A missing dependency summary cannot prove the issue is unblocked.
const blockedByUnknown = -1

// queueBlockers returns the open blockers shown for one ready issue. A listing
// snapshot that reports no open blockers needs no dependency read, and only a
// changed snapshot or count spends one again. The returned slice is shared and
// must not be modified.
func (s *Scheduler) queueBlockers(ctx context.Context, repo config.Repository, issue github.Issue) ([]github.Issue, error) {
	blockedBy := blockedByUnknown
	if issue.Dependencies != nil {
		blockedBy = issue.Dependencies.BlockedBy
	}
	key := issueKey(repo.Repo, issue.Number)
	if entry, ok := s.blockers.get(key); ok && entry.updatedAt.Equal(issue.UpdatedAt) && entry.blockedBy == blockedBy {
		return entry.blockers, nil
	}
	var blockers []github.Issue
	if blockedBy != 0 {
		var err error
		blockers, err = s.deps.GitHub.UnresolvedBlockers(ctx, repo.Repo, issue.Number)
		if err != nil {
			return nil, err
		}
	}
	s.blockers.put(key, blockerEntry{updatedAt: issue.UpdatedAt, blockedBy: blockedBy, blockers: blockers})
	return blockers, nil
}

// issueReadFailure is one refused issue whose GitHub read failed.
type issueReadFailure struct {
	number int
	err    error
}

// withRefusedIssues adds refused issues that no longer carry a Mergeyard label,
// so the dashboard keeps showing them and their reason until a trusted
// maintainer relabels them. Only refusals missing from the listing are read
// back. Refusals whose issue is closed or deleted are forgotten, so a stale row
// cannot be re-read on every refresh forever; the read-back stays bounded by
// the refusals that can still be shown.
func (s *Scheduler) withRefusedIssues(ctx context.Context, repo config.Repository, listed []github.Issue) ([]github.Issue, []issueReadFailure, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT issue_number FROM issue_refusals WHERE repository=? ORDER BY issue_number", strings.ToLower(repo.Repo))
	if err != nil {
		return nil, nil, err
	}
	seen := make(map[int]bool, len(listed))
	for _, issue := range listed {
		seen[issue.Number] = true
	}
	var missing []int
	for rows.Next() {
		var number int
		if err := rows.Scan(&number); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if !seen[number] {
			missing = append(missing, number)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	issues := append([]github.Issue(nil), listed...)
	var failed []issueReadFailure
	for _, number := range missing {
		issue, err := s.deps.GitHub.GetIssue(ctx, repo.Repo, number)
		var coded *fault.Error
		if errors.As(err, &coded) && coded.Code == "github.not_found" {
			// The issue is gone: no label can be removed and nothing can be
			// shown, so drop the row instead of re-reading it forever.
			if err := s.forgetRefusal(ctx, repo.Repo, number); err != nil {
				return nil, nil, err
			}
			continue
		}
		if err != nil {
			if githubFailure(err) {
				// Only this refusal is unreadable; keep the rest of the
				// repository's queue visible.
				failed = append(failed, issueReadFailure{number: number, err: err})
				continue
			}
			return nil, nil, err
		}
		if issue.State != github.Open {
			// The dashboard cannot show closed issues. A settled row would be
			// re-read on every refresh forever; a pending one stays for
			// reconciliation to finish its label removal.
			if err := s.forgetSettledRefusal(ctx, repo.Repo, number); err != nil {
				return nil, nil, err
			}
			continue
		}
		issues = append(issues, issue)
	}
	return issues, failed, nil
}

// QueueSnapshot is one dashboard discovery pass. Issues contains every
// readable repository's queue; Failures names the repositories or issues whose
// GitHub reads failed without hiding the rest.
type QueueSnapshot struct {
	Issues   []QueueIssue
	Failures []Finding
}

// Queue discovers issues without claiming them, even when paused or at capacity.
// It reads the poll's shared label-filtered listing, keeps refused issues
// visible without their ready label, and reuses blocker reads while an issue is
// unchanged. A dependency failure is returned rather than misrepresenting an
// issue as ready.
func (s *Scheduler) Queue(ctx context.Context) ([]QueueIssue, error) {
	snapshot, err := s.QueueReport(ctx)
	return snapshot.Issues, err
}

// QueueReport is Queue with the scoped GitHub failures that did not stop the
// remaining repositories from being discovered. Local-state failures stop the
// pass.
func (s *Scheduler) QueueReport(ctx context.Context) (QueueSnapshot, error) {
	snapshot := QueueSnapshot{Issues: []QueueIssue{}, Failures: []Finding{}}
	var scoped []error
	scopedFailure := func(code, repository string, issue int, err error) bool {
		failure := scopeGitHubFailure(code, repository, issue, "", err)
		if failure == nil {
			return false
		}
		snapshot.Failures = append(snapshot.Failures, failure.finding)
		scoped = append(scoped, failure)
		return true
	}
	for _, repo := range s.cfg.Repositories {
		if !repo.Enabled {
			continue
		}
		issues, err := s.listIssues(ctx, repo)
		if err != nil {
			if scopedFailure("reconcile.repository_failed", repo.Repo, 0, err) {
				continue
			}
			return snapshot, err
		}
		var refused []issueReadFailure
		issues, refused, err = s.withRefusedIssues(ctx, repo, issues)
		if err != nil {
			if scopedFailure("reconcile.repository_failed", repo.Repo, 0, err) {
				continue
			}
			return snapshot, err
		}
		for _, failure := range refused {
			// A refused issue is read back individually; one unreadable row
			// must not hide the rest of the repository's queue.
			scopedFailure("reconcile.issue_failed", repo.Repo, failure.number, failure.err)
		}
		issues = orderedIssues(issues)
		for _, issue := range issues {
			if issue.State != github.Open {
				continue
			}
			reason, err := s.refusalReason(ctx, repo.Repo, issue.Number)
			if err != nil {
				return snapshot, err
			}
			attention := hasLabel(issue, repo.Labels.NeedsAttention) || reason != ""
			if !attention && (!hasLabel(issue, repo.Labels.Ready) || hasLabel(issue, repo.Labels.Running)) {
				continue
			}
			item := QueueIssue{Repository: repo.Repo, Issue: issue, Attention: attention, RefusalReason: reason}
			if !attention {
				item.Blockers, err = s.queueBlockers(ctx, repo, issue)
				if err != nil {
					if scopedFailure("reconcile.issue_failed", repo.Repo, issue.Number, err) {
						continue
					}
					return snapshot, err
				}
			}
			snapshot.Issues = append(snapshot.Issues, item)
		}
	}
	return snapshot, errors.Join(scoped...)
}
