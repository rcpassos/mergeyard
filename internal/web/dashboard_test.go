package web

import (
	"testing"

	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
)

// A refresh that spends its budget waiting for a shared listing keeps the
// snapshot already on screen, along with its sync time, instead of showing a
// discovery error.
func TestApplyQueueReportKeepsLastSnapshotWhenStale(t *testing.T) {
	d := &dashboard{
		queue:      []scheduler.QueueIssue{{Repository: "owner/repo", Issue: github.Issue{Number: 7}}},
		queueTime:  "2026-10-18T12:00:00Z",
		queueReady: true,
	}
	d.applyQueueReport(scheduler.QueueSnapshot{
		Stale:  true,
		Issues: []scheduler.QueueIssue{{Repository: "owner/repo", Issue: github.Issue{Number: 8}}},
	}, nil)
	if len(d.queue) != 1 || d.queue[0].Issue.Number != 7 {
		t.Fatalf("stale refresh replaced the listing: %+v", d.queue)
	}
	if d.queueTime != "2026-10-18T12:00:00Z" {
		t.Fatalf("stale refresh changed the sync time: %q", d.queueTime)
	}
	if d.queueError != "" {
		t.Fatalf("stale refresh reported an error: %q", d.queueError)
	}
}

// A stale report with no earlier dashboard snapshot still shows the scheduler's
// listing without claiming a fresh sync.
func TestApplyQueueReportDoesNotStampAStaleFirstSnapshot(t *testing.T) {
	d := &dashboard{}
	d.applyQueueReport(scheduler.QueueSnapshot{
		Stale:  true,
		Issues: []scheduler.QueueIssue{{Repository: "owner/repo", Issue: github.Issue{Number: 7}}},
	}, nil)
	if len(d.queue) != 1 || d.queue[0].Issue.Number != 7 {
		t.Fatalf("stale first snapshot was lost: %+v", d.queue)
	}
	if d.queueTime != "" {
		t.Fatalf("stale first snapshot was stamped as fresh: %q", d.queueTime)
	}
	if d.queueError != "" {
		t.Fatalf("stale first snapshot reported an error: %q", d.queueError)
	}
}
