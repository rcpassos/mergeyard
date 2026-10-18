package scheduler

import (
	"testing"

	"github.com/rcpassos/mergeyard/internal/github"
)

// A listing fetch that began before a new poll must not replace that poll's
// snapshot: the dashboard refresh and the scheduler poll run concurrently.
func TestListingCacheDropsFetchesFromEarlierPolls(t *testing.T) {
	cache := &listingCache{}
	poll := cache.currentPoll()
	if !cache.store("owner/repo", listingEntry{issues: []github.Issue{{Number: 1}}, poll: poll}) {
		t.Fatal("current poll fetch was dropped")
	}
	cache.begin()
	if _, ok := cache.lookup("owner/repo"); ok {
		t.Fatal("begin did not invalidate the snapshot")
	}
	if cache.store("owner/repo", listingEntry{issues: []github.Issue{{Number: 1}}, poll: poll}) {
		t.Fatal("fetch from the previous poll was stored")
	}
	if _, ok := cache.lookup("owner/repo"); ok {
		t.Fatal("stale fetch overwrote the new poll")
	}
	next := cache.currentPoll()
	if next == poll {
		t.Fatal("begin did not advance the poll")
	}
	if !cache.store("owner/repo", listingEntry{issues: []github.Issue{{Number: 2}}, poll: next}) {
		t.Fatal("current poll fetch was dropped")
	}
	entry, ok := cache.lookup("owner/repo")
	if !ok || len(entry.issues) != 1 || entry.issues[0].Number != 2 {
		t.Fatalf("lookup = %+v, %v", entry, ok)
	}
}
