package scheduler

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/github"
)

// keyedCache is a concurrency-safe map of discovery snapshots keyed by
// repository or issue. Callers own the entry policy.
type keyedCache[T any] struct {
	mu      sync.Mutex
	entries map[string]T
}

func (c *keyedCache[T]) get(key string) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	return entry, ok
}

func (c *keyedCache[T]) put(key string, entry T) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]T)
	}
	c.entries[key] = entry
}

func (c *keyedCache[T]) delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// listingCache is the shared per-poll snapshot of each repository's open
// Mergeyard-labelled issues. Reconcile, dispatch, plan and the dashboard queue
// read the same snapshot, so one poll lists a repository once instead of paging
// through every open issue and pull request for each reader.
type listingCache struct {
	mu      sync.Mutex
	poll    uint64
	entries map[string]listingEntry
}

// listingEntry is one repository's snapshot of a poll. A fetch is stamped with
// the poll that started it and the time it started, so a slow fetch can neither
// pass for a newer poll nor look fresher than it is.
type listingEntry struct {
	issues    []github.Issue
	fetchedAt time.Time
	poll      uint64
}

// begin starts a new poll and invalidates every snapshot.
func (c *listingCache) begin() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.poll++
	c.entries = nil
}

func (c *listingCache) currentPoll() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.poll
}

func (c *listingCache) lookup(key string) (listingEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	return entry, ok
}

// store keeps a completed fetch only while it still belongs to the current
// poll, so a fetch that began before a new poll cannot replace that poll's
// listing.
func (c *listingCache) store(key string, entry listingEntry) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry.poll != c.poll {
		return false
	}
	if c.entries == nil {
		c.entries = make(map[string]listingEntry)
	}
	c.entries[key] = entry
	return true
}

// beginPoll starts a new scheduler poll, invalidating the previous snapshots
// while readers within the same poll keep sharing the refreshed listing.
func (s *Scheduler) beginPoll() {
	s.listings.begin()
}

// pollLabels names the labels that place an issue on Mergeyard's radar.
func pollLabels(repo config.Repository) []string {
	return []string{repo.Labels.Ready, repo.Labels.Running, repo.Labels.NeedsAttention}
}

func (s *Scheduler) pollInterval() time.Duration {
	if s.cfg.PollInterval <= 0 {
		return 30 * time.Second
	}
	return s.cfg.PollInterval
}

// listingWindow is how long a snapshot stays reusable without a fresh read.
// One poll interval covers the scheduler's own refresh; the second absorbs
// timer skew between the scheduler and the dashboard, whose identical tickers
// would otherwise both fetch at every boundary. A dashboard without a running
// scheduler refreshes its own listing once the window expires.
func (s *Scheduler) listingWindow() time.Duration {
	return 2 * s.pollInterval()
}

// refusalWindow is how long a refused issue's last readable snapshot is reused
// without another GitHub read. A refusal removes the ready label, so the
// label-filtered listing never includes the issue again until a maintainer
// relabels it; unlike a poll listing, this snapshot must survive many polls.
// Rereading every refused issue on every refresh would spend the shared hourly
// REST budget on rows that usually change only when a human intervenes.
func (s *Scheduler) refusalWindow() time.Duration {
	return 15 * time.Minute
}

// listingWaitError reports that a caller stopped waiting for a shared listing
// fetch before it could start. It keeps the context error for callers that
// distinguish a spent budget from a failed GitHub request.
type listingWaitError struct{ err error }

func (e *listingWaitError) Error() string { return e.err.Error() }
func (e *listingWaitError) Unwrap() error { return e.err }

// listIssues returns the poll's label-filtered listing for one repository,
// fetching it on first use. The returned slice is shared and must not be
// modified. Concurrent readers share one fetch per repository through
// listingFetch instead of racing duplicates, and a reader that gives up while
// waiting is released instead of fetching again with an expired context.
func (s *Scheduler) listIssues(ctx context.Context, repo config.Repository) ([]github.Issue, error) {
	key := strings.ToLower(repo.Repo)
	if issues, ok := s.cachedListing(key); ok {
		return issues, nil
	}
	if err := s.listingFetch.Lock(ctx); err != nil {
		return nil, &listingWaitError{err: err}
	}
	defer s.listingFetch.Unlock()
	// The gate can be released at the same moment the caller gives up. Do not
	// start a request that already has an expired context.
	if err := ctx.Err(); err != nil {
		return nil, &listingWaitError{err: err}
	}
	// Another reader may have fetched while we waited for the lock.
	if issues, ok := s.cachedListing(key); ok {
		return issues, nil
	}
	poll, fetchedAt := s.listings.currentPoll(), s.deps.Now()
	issues, err := s.deps.GitHub.ListLabeledIssues(ctx, repo.Repo, pollLabels(repo))
	if err != nil {
		return nil, err
	}
	s.listings.store(key, listingEntry{issues: issues, fetchedAt: fetchedAt, poll: poll})
	return issues, nil
}

func (s *Scheduler) cachedListing(key string) ([]github.Issue, bool) {
	entry, ok := s.listings.lookup(key)
	if !ok || s.deps.Now().Sub(entry.fetchedAt) >= s.listingWindow() {
		return nil, false
	}
	return entry.issues, true
}
