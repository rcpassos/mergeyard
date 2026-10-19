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

// refusedIssueSnapshot is the last readable GitHub snapshot of one refused
// issue that the label-filtered listing no longer includes, and when it was
// read. It survives polls because a refusal removes the ready label. A refused
// issue with no readable snapshot yet keeps its read failure instead, so an
// outage does not spend one call per refused issue on every refresh.
type refusedIssueSnapshot struct {
	issue     github.Issue
	fetchedAt time.Time
	err       error
}

// issueNotFound reports GitHub's conclusive answer that an issue no longer
// exists.
func issueNotFound(err error) bool {
	var coded *fault.Error
	return errors.As(err, &coded) && coded.Code == "github.not_found"
}

// refusalReadWindow bounds one refused-issue read. A refusal is display-only,
// so a slow request must not consume the dashboard's whole discovery budget.
const refusalReadWindow = 5 * time.Second

// refusalReadContext bounds one refused-issue read to at most half of the
// caller's remaining discovery budget, leaving room for the rest of the pass.
func refusalReadContext(ctx context.Context) (context.Context, context.CancelFunc) {
	window := refusalReadWindow
	if deadline, ok := ctx.Deadline(); ok {
		if half := time.Until(deadline) / 2; half < window {
			window = half
		}
	}
	return context.WithTimeout(ctx, window)
}

// refusedReadTimedOut distinguishes the dashboard refresh deadline from a
// shutdown or disconnected caller. The GitHub adapter reports an expired
// deadline as github.timeout and any other cancellation as github.canceled.
func refusedReadTimedOut(err error) bool {
	return githubFaultCode(err) == "github.timeout" || errors.Is(err, context.DeadlineExceeded)
}

// normalizeRefusedReadError turns a raw deadline into the GitHub adapter's
// timeout code so the failure stays scoped to its refused issue.
func normalizeRefusedReadError(err error) error {
	if refusedReadTimedOut(err) && !githubFailure(err) {
		return &fault.Error{Code: "github.timeout", Err: err}
	}
	return err
}

// refusedReadCanceled reports a caller that stopped the pass rather than the
// dashboard refresh deadline. Those failures must not be cached as the issue's
// state; the next refresh retries them.
func refusedReadCanceled(ctx context.Context, err error) bool {
	return ctx.Err() != nil && !refusedReadTimedOut(err)
}

// withRefusedIssues adds refused issues that no longer carry a Mergeyard label,
// so the dashboard keeps showing them and their reason until a trusted
// maintainer relabels them. Only refusals missing from the listing are read
// back, and a readable snapshot is reused across refreshes until the refusal
// row changes or the window expires. Refusals whose issue is closed or deleted
// are forgotten when that snapshot is refreshed, so a stale row cannot be
// re-read on every refresh forever.
func (s *Scheduler) withRefusedIssues(ctx context.Context, repo config.Repository, listed []github.Issue) ([]github.Issue, []issueReadFailure, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT issue_number FROM issue_refusals WHERE repository=? ORDER BY issue_number", strings.ToLower(repo.Repo))
	if err != nil {
		return nil, nil, err
	}
	seen := make(map[int]bool, len(listed))
	for _, issue := range listed {
		seen[issue.Number] = true
		// The label-filtered listing is authoritative: a relabelled issue is
		// current and its old snapshot must not outlive the listing entry.
		s.refusalSnapshots.delete(issueKey(repo.Repo, issue.Number))
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
		issue, err := s.refusedIssue(ctx, repo, number)
		if issueNotFound(err) {
			// The issue is gone: no label can be removed and nothing can be
			// shown, so drop the row instead of re-reading it forever.
			if err := s.forgetRefusal(ctx, repo.Repo, number); err != nil {
				return nil, nil, err
			}
			continue
		}
		if err != nil {
			err = normalizeRefusedReadError(err)
			if ctx.Err() != nil && !refusedReadTimedOut(err) {
				// Shutdown or a disconnected caller: stop the pass instead of
				// pretending this issue's read was a scoped failure.
				return nil, nil, ctx.Err()
			}
			if !githubFailure(err) {
				return nil, nil, err
			}
			// Only this refusal is unreadable; keep the rest of the
			// repository's queue visible.
			failed = append(failed, issueReadFailure{number: number, err: err})
			if ctx.Err() != nil {
				// The refresh deadline is spent. The read is already cached as
				// a failure, so the next refresh backs off instead of retrying
				// it, and the caller can still show what it discovered.
				break
			}
			continue
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

// refusedIssue returns a refused issue's last readable snapshot. It reads
// GitHub only when no usable snapshot is cached. A failed read keeps the last
// snapshot, or records the failure when there is none, and backs off for a full
// window so a rate limit or outage does not turn every dashboard refresh into
// another failing call.
func (s *Scheduler) refusedIssue(ctx context.Context, repo config.Repository, number int) (github.Issue, error) {
	key := issueKey(repo.Repo, number)
	if entry, ok := s.cachedRefusedIssue(key); ok {
		return entry.issue, entry.err
	}
	if err := s.refusalFetch.Lock(ctx); err != nil {
		return github.Issue{}, normalizeRefusedReadError(err)
	}
	defer s.refusalFetch.Unlock()
	// Another reader may have read it while we waited for the gate.
	if entry, ok := s.cachedRefusedIssue(key); ok {
		return entry.issue, entry.err
	}
	now := s.deps.Now()
	readCtx, cancel := refusalReadContext(ctx)
	issue, err := s.deps.GitHub.GetIssue(readCtx, repo.Repo, number)
	cancel()
	if err != nil {
		// A missing issue is a conclusive answer, not a failed read: the row
		// must be forgotten rather than kept visible from a stale snapshot.
		if issueNotFound(err) {
			return github.Issue{}, err
		}
		err = normalizeRefusedReadError(err)
		if entry, ok := s.refusalSnapshots.get(key); ok && entry.err == nil {
			// Keep the last readable snapshot. A deadline or ordinary failure
			// backs off; a canceled caller keeps the old read time so the next
			// pass retries it.
			if !refusedReadCanceled(ctx, err) {
				entry.fetchedAt = now
				s.refusalSnapshots.put(key, entry)
			}
			return entry.issue, nil
		}
		if refusedReadCanceled(ctx, err) || !githubFailure(err) {
			// A canceled pass or local failure must not be remembered as the
			// issue's state; the next refresh retries it.
			return github.Issue{}, err
		}
		// No snapshot to show: remember the failure so the next refreshes back
		// off, but keep returning it so the issue stays in needs-attention.
		s.refusalSnapshots.put(key, refusedIssueSnapshot{fetchedAt: now, err: err})
		return github.Issue{}, err
	}
	s.refusalSnapshots.put(key, refusedIssueSnapshot{issue: issue, fetchedAt: now})
	return issue, nil
}

func (s *Scheduler) cachedRefusedIssue(key string) (refusedIssueSnapshot, bool) {
	entry, ok := s.refusalSnapshots.get(key)
	if !ok || s.deps.Now().Sub(entry.fetchedAt) >= s.refusalWindow() {
		return refusedIssueSnapshot{}, false
	}
	return entry, true
}

// QueueSnapshot is one dashboard discovery pass. Issues contains every
// readable repository's queue; Failures names the repositories or issues whose
// GitHub reads failed without hiding the rest. Stale marks a copy of the last
// usable pass that is returned when the caller's deadline expires before a
// shared listing can refresh.
type QueueSnapshot struct {
	Issues   []QueueIssue
	Failures []Finding
	Stale    bool
}

// cloneQueueSnapshot copies a snapshot's slices. QueueIssue values and the
// blocker slices they share are immutable after discovery.
func cloneQueueSnapshot(snapshot QueueSnapshot) QueueSnapshot {
	return QueueSnapshot{
		Issues:   append([]QueueIssue(nil), snapshot.Issues...),
		Failures: append([]Finding(nil), snapshot.Failures...),
	}
}

// rememberQueue keeps the last usable discovery pass for a refresh that runs
// out of budget waiting for a shared listing.
func (s *Scheduler) rememberQueue(snapshot QueueSnapshot) {
	kept := cloneQueueSnapshot(snapshot)
	s.queueMu.Lock()
	s.lastQueue = &kept
	s.queueMu.Unlock()
}

// staleQueueSnapshot returns the last usable discovery pass, marked stale so
// a caller can tell it did not refresh.
func (s *Scheduler) staleQueueSnapshot() (QueueSnapshot, bool) {
	s.queueMu.RLock()
	defer s.queueMu.RUnlock()
	if s.lastQueue == nil {
		return QueueSnapshot{}, false
	}
	snapshot := cloneQueueSnapshot(*s.lastQueue)
	snapshot.Stale = true
	return snapshot, true
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
			// The shared listing may be held by a slow fetch. Once the
			// caller's budget is spent waiting, keep the last usable queue
			// visible instead of turning the wait into a discovery failure.
			var waited *listingWaitError
			if errors.As(err, &waited) && errors.Is(ctx.Err(), context.DeadlineExceeded) {
				if previous, ok := s.staleQueueSnapshot(); ok {
					return previous, nil
				}
			}
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
	err := errors.Join(scoped...)
	if err == nil || githubFailure(err) {
		s.rememberQueue(snapshot)
	}
	return snapshot, err
}
