package scheduler

import (
	"context"
	"errors"
	"sort"
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

// discovery is the budget of one repository's dashboard discovery: the context
// for the calls that reach GitHub, the context for the local lookups that
// must not share the GitHub budget, and the instant after which the
// repository's refused issues are no longer read back.
type discovery struct {
	github    context.Context
	local     context.Context
	allowance time.Time
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
const localBudget = 5 * time.Second

// shareOf returns the given share of a duration.
func shareOf(duration time.Duration, share float64) time.Duration {
	return time.Duration(float64(duration) * share)
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

// refusalReadWindowFor returns the time one refused-issue read may take, and
// whether the caller's own budget cut it short. A read the budget cut short
// is merely slow: it is read again on the next refresh instead of being
// remembered as the issue's failure for a whole window. The allowance bounds
// the read, so a refresh cannot spend more on refused issues than it reserved
// for them.
func refusalReadWindowFor(ctx context.Context, allowance time.Time) (time.Duration, bool) {
	window, cutShort := refusalReadWindow, false
	if deadline, ok := ctx.Deadline(); ok {
		if half := time.Until(deadline) / 2; half < window {
			window, cutShort = half, true
		}
	}
	if !allowance.IsZero() {
		if left := time.Until(allowance); left < window {
			window, cutShort = left, true
		}
	}
	return window, cutShort
}

// refusalAllowance returns the instant after which a repository stops
// re-reading refused issues. The repository may spend a share of what is left
// of the refresh budget, split among the repositories this pass has not
// discovered yet, and never more than the pass's own deadline for refused
// reads. The budget is the caller's own deadline, so it is measured against
// the wall clock rather than the scheduler's injected clock.
func refusalAllowance(ctx context.Context, refusalDeadline time.Time, reposLeft int) time.Time {
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
	allowance := time.Now().Add(shareOf(remaining, refusalReadShare) / time.Duration(reposLeft))
	if !refusalDeadline.IsZero() && allowance.After(refusalDeadline) {
		return refusalDeadline
	}
	return allowance
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
// read the caller's own budget cut short backs off for one refresh, so a
// healthy issue is not hidden as a timeout for a whole window.
func (s *Scheduler) refusalBackoff(err error, cutShort bool) time.Duration {
	if cutShort && refusedReadTimedOut(err) {
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
// the queue. The refusals are visited in turn, so a slow one cannot always
// precede the refusals behind it and keep them out of the queue forever.
func (s *Scheduler) withRefusedIssues(budget discovery, repo config.Repository, listed []github.Issue) ([]github.Issue, []issueReadFailure, error) {
	rows, err := s.db.QueryContext(budget.local, "SELECT issue_number FROM issue_refusals WHERE repository=? ORDER BY issue_number", strings.ToLower(repo.Repo))
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
	for _, number := range s.refusalOrder(missing, repo.Repo) {
		key := issueKey(repo.Repo, number)
		entry, usable := s.cachedRefusal(key)
		if usable {
			// The snapshot is still usable: no read is needed.
			issues, failed = addRefusal(issues, failed, number, entry)
			continue
		}
		if refusedReadCanceled(budget.github) {
			// Shutdown or a disconnected caller: stop the pass instead of
			// pretending this issue's read was a scoped failure.
			return nil, nil, budget.github.Err()
		}
		if !refusalReadAllowed(budget.github, budget.allowance) {
			// The pass cannot afford another read. The last readable snapshot
			// still describes the issue, so the queue keeps it, and an issue
			// whose last read failed keeps that failure. An issue that was
			// never read stays out of the queue until a later refresh reads
			// it.
			issues, failed = addRefusal(issues, failed, number, entry)
			continue
		}
		// The refusal is read now, so the next pass starts after it. A slow
		// refusal that spends the repository's share must not keep the
		// refusals behind it out of the queue for good.
		s.setRefusalCursor(repo.Repo, number)
		issue, err := s.readRefusedIssue(budget, repo, number)
		if issueNotFound(err) {
			// The issue is gone: no label can be removed and nothing can be
			// shown, so drop the row instead of re-reading it forever.
			if err := s.forgetRefusal(budget.local, repo.Repo, number); err != nil {
				return nil, nil, err
			}
			continue
		}
		if err != nil {
			err = normalizeRefusedReadError(budget.github, err)
			if refusedReadCanceled(budget.github) {
				// Shutdown or a disconnected caller: stop the pass instead of
				// pretending this issue's read was a scoped failure.
				return nil, nil, budget.github.Err()
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
			if err := s.forgetSettledRefusal(budget.local, repo.Repo, number); err != nil {
				return nil, nil, err
			}
			continue
		}
		issues = append(issues, issue)
	}
	return issues, failed, nil
}

// cachedRefusal returns one refused issue's cached snapshot, and whether it
// is still usable. A nil snapshot means the issue was never read.
func (s *Scheduler) cachedRefusal(key string) (*refusedIssueSnapshot, bool) {
	entry, ok := s.refusalSnapshots.get(key)
	if !ok {
		return nil, false
	}
	return &entry, s.deps.Now().Before(entry.expiresAt)
}

// refusalOrder returns the refusals to visit, starting after the repository's
// cursor. Every pass reads the refusals in turn, so a refusal that always
// spends the repository's share cannot keep the refusals behind it out of the
// queue for good.
func (s *Scheduler) refusalOrder(missing []int, repo string) []int {
	cursor, _ := s.refusalCursors.get(strings.ToLower(repo))
	index := sort.SearchInts(missing, cursor+1)
	if index == 0 {
		return missing
	}
	order := make([]int, 0, len(missing))
	order = append(order, missing[index:]...)
	order = append(order, missing[:index]...)
	return order
}

// setRefusalCursor records the refusal the pass last read, so the next pass
// starts after it.
func (s *Scheduler) setRefusalCursor(repo string, number int) {
	s.refusalCursors.put(strings.ToLower(repo), number)
}

// addRefusal adds one refused issue to the pass: its readable snapshot, or the
// failure remembered for it. A snapshot that was never read adds nothing.
func addRefusal(issues []github.Issue, failed []issueReadFailure, number int, entry *refusedIssueSnapshot) ([]github.Issue, []issueReadFailure) {
	if entry == nil {
		return issues, failed
	}
	if entry.err != nil {
		return issues, append(failed, issueReadFailure{number: number, err: entry.err})
	}
	return append(issues, entry.issue), failed
}

// readRefusedIssue reads one refused issue from GitHub and caches what it
// learned. It reads GitHub only when no usable snapshot is cached. A failed
// read keeps the last snapshot, or records the failure when there is none,
// and backs off so a rate limit or outage does not turn every dashboard
// refresh into another failing call.
func (s *Scheduler) readRefusedIssue(budget discovery, repo config.Repository, number int) (github.Issue, error) {
	key := issueKey(repo.Repo, number)
	if err := s.refusalFetch.Lock(budget.github); err != nil {
		return github.Issue{}, normalizeRefusedReadError(budget.github, err)
	}
	defer s.refusalFetch.Unlock()
	// Another reader may have read it while we waited for the gate.
	if entry, usable := s.cachedRefusal(key); usable {
		return entry.issue, entry.err
	}
	now := s.deps.Now()
	window, cutShort := refusalReadWindowFor(budget.github, budget.allowance)
	readCtx, cancel := context.WithTimeout(budget.github, window)
	issue, err := s.deps.GitHub.GetIssue(readCtx, repo.Repo, number)
	cancel()
	if err != nil {
		// The read is over, so the failure happened now: its backoff starts
		// here rather than when the read began.
		now = s.deps.Now()
		// A missing issue is a conclusive answer, not a failed read: the row
		// must be forgotten rather than kept visible from a stale snapshot.
		if issueNotFound(err) {
			return github.Issue{}, err
		}
		err = normalizeRefusedReadError(budget.github, err)
		if entry, ok := s.refusalSnapshots.get(key); ok && entry.err == nil {
			// Keep the last readable snapshot. A deadline or ordinary failure
			// backs off; a canceled caller keeps the old expiry so the next
			// pass retries it.
			if !refusedReadCanceled(budget.github) {
				entry.expiresAt = now.Add(s.refusalBackoff(err, cutShort))
				s.refusalSnapshots.put(key, entry)
			}
			return entry.issue, nil
		}
		if refusedReadCanceled(budget.github) || !githubFailure(err) {
			// A canceled pass or local failure must not be remembered as the
			// issue's state; the next refresh retries it.
			return github.Issue{}, err
		}
		// No snapshot to show: remember the failure so the next refreshes back
		// off, but keep returning it so the issue stays in needs-attention.
		s.refusalSnapshots.put(key, refusedIssueSnapshot{expiresAt: now.Add(s.refusalBackoff(err, cutShort)), err: err})
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
	// The refused reads may spend at most half of the refresh budget. The
	// rest is reserved for the remaining listings, the blocker reads, and the
	// assembly of the queue.
	var refusalDeadline time.Time
	if deadline, ok := ctx.Deadline(); ok {
		refusalDeadline = time.Now().Add(shareOf(time.Until(deadline), refusalReadShare))
	}
	for index, repo := range repositories {
		issues, err := s.listIssues(ctx, repo)
		if err != nil {
			// The shared listing may be held by a slow fetch. Once the
			// caller's budget is spent waiting, keep the last usable queue
			// visible instead of turning the wait into a discovery failure.
			var waited *listingWaitError
			if errors.As(err, &waited) && budgetSpent(ctx) {
				if previous, ok := s.staleQueueSnapshot(); ok {
					return previous, nil
				}
				// No earlier queue to keep: report the repository's failure
				// and let the rest of the pass carry on.
				if scopedFailure("reconcile.repository_failed", repo.Repo, 0, &fault.Error{Code: "github.timeout", Err: err}) {
					continue
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
		budget := discovery{
			github:    ctx,
			local:     local,
			allowance: refusalAllowance(ctx, refusalDeadline, len(repositories)-index),
		}
		issues, refused, err = s.withRefusedIssues(budget, repo, issues)
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
