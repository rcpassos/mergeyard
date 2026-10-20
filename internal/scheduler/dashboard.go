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
// issue that the label-filtered listing no longer includes, and how long it
// stays usable. It survives polls because a refusal removes the ready label.
// A refused issue with no readable snapshot yet keeps its read failure
// instead, so an outage does not spend one call per refused issue on every
// refresh.
type refusedIssueSnapshot struct {
	issue     github.Issue
	expiresAt time.Time
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

// refusalReadShare is the share of the refresh budget that refused-issue
// re-reads may spend. The rest is reserved for the remaining repositories'
// listings and for the pass's own work, so a repository full of refused
// issues cannot spend the whole refresh and leave the other repositories
// without a queue.
const refusalReadShare = 0.5

// localBudget is the extra time the pass's local database work may use once
// the caller's GitHub budget is spent. The lookups are local and fast, and
// they must not fail because the budget reserved for GitHub is gone.
const localBudget = 10 * time.Second

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

// budgetSpent reports that the caller's refresh deadline expired. The pass
// keeps running with the snapshots it already holds: a spent budget is not a
// cancellation.
func budgetSpent(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.DeadlineExceeded)
}

// refusedReadTimedOut distinguishes the dashboard refresh deadline from a
// shutdown or disconnected caller. The GitHub adapter reports an expired
// deadline as github.timeout and any other cancellation as github.canceled.
func refusedReadTimedOut(err error) bool {
	return githubFaultCode(err) == "github.timeout" || errors.Is(err, context.DeadlineExceeded)
}

// normalizeRefusedReadError turns a raw deadline into the GitHub adapter's
// timeout code so the failure stays scoped to its refused issue. A read the
// caller's spent budget stopped is a timeout too, not a cancellation.
func normalizeRefusedReadError(ctx context.Context, err error) error {
	if refusedReadTimedOut(err) && !githubFailure(err) {
		return &fault.Error{Code: "github.timeout", Err: err}
	}
	if budgetSpent(ctx) && githubFaultCode(err) == "github.canceled" {
		return &fault.Error{Code: "github.timeout", Err: err}
	}
	return err
}

// refusedReadCanceled reports a caller that stopped the pass rather than the
// dashboard refresh deadline. Those failures must not be cached as the
// issue's state; the next refresh retries them.
func refusedReadCanceled(ctx context.Context) bool {
	return ctx.Err() != nil && !budgetSpent(ctx)
}

// refusalAllowance returns the instant after which this pass stops re-reading
// refused issues for one repository. The re-reads get a share of what is left
// of the refresh budget, split among the repositories that are not discovered
// yet, so one repository's refusals cannot spend the whole refresh.
func (s *Scheduler) refusalAllowance(ctx context.Context, reposLeft int) time.Time {
	deadline, ok := ctx.Deadline()
	if !ok {
		return time.Time{}
	}
	if reposLeft < 1 {
		reposLeft = 1
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return time.Now()
	}
	share := time.Duration(float64(remaining) * refusalReadShare / float64(reposLeft))
	return time.Now().Add(share)
}

// refusalReadAllowed reports whether a refused-issue re-read may start. The
// caller must still have budget, and this repository's share of it must not be
// spent: a refresh re-reads as many refusals as it can afford and keeps the
// rest from their last readable snapshot.
func refusalReadAllowed(ctx context.Context, allowance time.Time) bool {
	if ctx.Err() != nil {
		return false
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return false
	}
	if !allowance.IsZero() && !time.Now().Before(allowance) {
		return false
	}
	return true
}

// refusalBackoff is how long a failed refused-issue read is remembered before
// the issue is read again. A GitHub failure backs off for the full window, so
// an outage does not spend one call per refused issue on every refresh. A
// read that was merely slow backs off for one refresh, so a healthy issue is
// not hidden as a timeout for a whole window.
func (s *Scheduler) refusalBackoff(err error) time.Duration {
	if refusedReadTimedOut(err) {
		return s.pollInterval()
	}
	return s.refusalWindow()
}

// localContext returns a context for the pass's local database work. The
// lookups are local and must not fail because the caller's GitHub budget is
// spent, so the caller's deadline does not apply to them; they may overrun it
// by the local budget. A shutdown or a disconnected caller still stops them.
func localContext(ctx context.Context) (context.Context, context.CancelFunc) {
	parent := context.WithoutCancel(ctx)
	var release func()
	if deadline, ok := ctx.Deadline(); ok {
		parent, release = context.WithDeadline(parent, deadline.Add(localBudget))
	}
	local, cancel := context.WithCancel(parent)
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if !budgetSpent(ctx) {
				cancel()
			}
		case <-stopped:
		}
	}()
	return local, func() {
		close(stopped)
		cancel()
		if release != nil {
			release()
		}
	}
}

// withRefusedIssues adds refused issues that no longer carry a Mergeyard label,
// so the dashboard keeps showing them and their reason until a trusted
// maintainer relabels them. Only refusals missing from the listing are read
// back, and a readable snapshot is reused across refreshes until the refusal
// row changes or the window expires. Refusals whose issue is closed or deleted
// are forgotten when that snapshot is refreshed, so a stale row cannot be
// re-read on every refresh forever.
//
// A re-read spends this repository's share of the refresh budget. Once that
// share is spent the pass stops reading and keeps the last readable snapshot
// of the refusals it did not reach, so a window full of refusals cannot hide
// the queue.
func (s *Scheduler) withRefusedIssues(ctx context.Context, local context.Context, repo config.Repository, listed []github.Issue, allowance time.Time) ([]github.Issue, []issueReadFailure, error) {
	rows, err := s.db.QueryContext(local, "SELECT issue_number FROM issue_refusals WHERE repository=? ORDER BY issue_number", strings.ToLower(repo.Repo))
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
		key := issueKey(repo.Repo, number)
		entry, cached := s.refusalSnapshots.get(key)
		if cached && s.deps.Now().Before(entry.expiresAt) {
			if entry.err != nil {
				// The last read failed and is still remembered. Report it
				// without spending another call.
				failed = append(failed, issueReadFailure{number: number, err: entry.err})
				continue
			}
			issues = append(issues, entry.issue)
			continue
		}
		if !refusalReadAllowed(ctx, allowance) {
			// The pass cannot afford another read. The last readable snapshot
			// still describes the issue, so the queue keeps it; an issue whose
			// last read failed keeps that failure until it is read back.
			if !cached {
				continue
			}
			if entry.err != nil {
				failed = append(failed, issueReadFailure{number: number, err: entry.err})
				continue
			}
			issues = append(issues, entry.issue)
			continue
		}
		issue, err := s.readRefusedIssue(ctx, repo, number)
		if issueNotFound(err) {
			// The issue is gone: no label can be removed and nothing can be
			// shown, so drop the row instead of re-reading it forever.
			if err := s.forgetRefusal(local, repo.Repo, number); err != nil {
				return nil, nil, err
			}
			continue
		}
		if err != nil {
			err = normalizeRefusedReadError(ctx, err)
			if refusedReadCanceled(ctx) {
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
			continue
		}
		if issue.State != github.Open {
			// The dashboard cannot show closed issues. A settled row would be
			// re-read on every refresh forever; a pending one stays for
			// reconciliation to finish its label removal.
			if err := s.forgetSettledRefusal(local, repo.Repo, number); err != nil {
				return nil, nil, err
			}
			continue
		}
		issues = append(issues, issue)
	}
	return issues, failed, nil
}

// readRefusedIssue reads one refused issue from GitHub and caches what it
// learned. It reads GitHub only when no usable snapshot is cached. A failed
// read keeps the last snapshot, or records the failure when there is none,
// and backs off so a rate limit or outage does not turn every dashboard
// refresh into another failing call.
func (s *Scheduler) readRefusedIssue(ctx context.Context, repo config.Repository, number int) (github.Issue, error) {
	key := issueKey(repo.Repo, number)
	if err := s.refusalFetch.Lock(ctx); err != nil {
		return github.Issue{}, normalizeRefusedReadError(ctx, err)
	}
	defer s.refusalFetch.Unlock()
	// Another reader may have read it while we waited for the gate.
	if entry, ok := s.refusalSnapshots.get(key); ok && s.deps.Now().Before(entry.expiresAt) {
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
		err = normalizeRefusedReadError(ctx, err)
		if entry, ok := s.refusalSnapshots.get(key); ok && entry.err == nil {
			// Keep the last readable snapshot. A deadline or ordinary failure
			// backs off; a canceled caller keeps the old expiry so the next
			// pass retries it.
			if !refusedReadCanceled(ctx) {
				entry.expiresAt = now.Add(s.refusalBackoff(err))
				s.refusalSnapshots.put(key, entry)
			}
			return entry.issue, nil
		}
		if refusedReadCanceled(ctx) || !githubFailure(err) {
			// A canceled pass or local failure must not be remembered as the
			// issue's state; the next refresh retries it.
			return github.Issue{}, err
		}
		// No snapshot to show: remember the failure so the next refreshes back
		// off, but keep returning it so the issue stays in needs-attention.
		s.refusalSnapshots.put(key, refusedIssueSnapshot{expiresAt: now.Add(s.refusalBackoff(err)), err: err})
		return github.Issue{}, err
	}
	s.refusalSnapshots.put(key, refusedIssueSnapshot{issue: issue, expiresAt: now.Add(s.refusalWindow())})
	return issue, nil
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
//
// The caller's budget bounds the calls that reach GitHub. The refused issues
// are read back within a share of that budget, so a window full of refusals
// cannot spend the whole refresh; the refusals the pass cannot afford keep
// their last readable snapshot. The local lookups that assemble the queue do
// not share the GitHub budget, so a spent budget costs the refresh its fresh
// refusals rather than its whole queue.
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
	repositories := make([]config.Repository, 0, len(s.cfg.Repositories))
	for _, repo := range s.cfg.Repositories {
		if repo.Enabled {
			repositories = append(repositories, repo)
		}
	}
	// The pass's database lookups must not fail because the caller's GitHub
	// budget is spent, so they run on a context of their own.
	local, stopLocal := localContext(ctx)
	defer stopLocal()
	for index, repo := range repositories {
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
		// The refused re-reads get a share of what is left of the refresh
		// budget, so the repositories that follow still fit in it.
		allowance := s.refusalAllowance(ctx, len(repositories)-index)
		issues, refused, err = s.withRefusedIssues(ctx, local, repo, issues, allowance)
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
			reason, err := s.refusalReason(local, repo.Repo, issue.Number)
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
