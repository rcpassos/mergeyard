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
// repository or issue. Callers own the entry policy: freshness for the poll
// listing, snapshot matching for dashboard blocker reuse.
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

func (c *keyedCache[T]) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
}

// listingEntry is the shared per-poll snapshot of one repository's open
// Mergeyard-labelled issues. Reconcile, dispatch, plan and the dashboard queue
// read the same snapshot, so one poll lists a repository once instead of paging
// through every open issue and pull request for each reader.
type listingEntry struct {
	issues    []github.Issue
	fetchedAt time.Time
}

// beginPoll invalidates every repository snapshot so the poll refreshes it,
// while readers within the same poll keep sharing the refreshed listing.
func (s *Scheduler) beginPoll() {
	s.listings.clear()
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

// listIssues returns the poll's label-filtered listing for one repository,
// fetching it on first use. The returned slice is shared and must not be
// modified. A reader outside a scheduler poll, such as the dashboard queue
// without a running scheduler, refreshes a snapshot older than the poll
// interval so the dashboard still observes GitHub.
func (s *Scheduler) listIssues(ctx context.Context, repo config.Repository) ([]github.Issue, error) {
	key := strings.ToLower(repo.Repo)
	if entry, ok := s.listings.get(key); ok && s.deps.Now().Sub(entry.fetchedAt) < s.pollInterval() {
		return entry.issues, nil
	}
	issues, err := s.deps.GitHub.ListLabeledIssues(ctx, repo.Repo, pollLabels(repo))
	if err != nil {
		return nil, err
	}
	s.listings.put(key, listingEntry{issues: issues, fetchedAt: s.deps.Now()})
	return issues, nil
}
