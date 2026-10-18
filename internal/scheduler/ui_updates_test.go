package scheduler_test

import (
	"context"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/doctor"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/web"
)

// Dashboard discovery notifications are live-only: they must still reach
// subscribed browsers, but they are UI state, not durable run history.
func TestDashboardUpdatesReachSubscribersWithoutPersisting(t *testing.T) {
	s, rt := fixture(t, &fakeGitHub{issues: map[string][]github.Issue{}})
	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.PollInterval = 10 * time.Millisecond
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{
		DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg,
		Diagnostics: func(context.Context) doctor.Report { return doctor.Report{} },
	})
	if err != nil {
		t.Fatal(err)
	}
	live, cancel := rt.Events.Subscribe(4096)
	defer cancel()
	ctx, cancelRun := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); server.RunUpdates(ctx) }()
	defer func() { cancelRun(); <-done }()
	seen := map[string]bool{}
	deadline := time.After(10 * time.Second)
	for len(seen) < 2 {
		select {
		case event := <-live:
			if event.Type != "queue.updated" && event.Type != "diagnostics.updated" {
				t.Fatalf("unexpected live event: %+v", event)
			}
			if event.ID != 0 {
				t.Fatalf("UI-only update was persisted: %+v", event)
			}
			seen[event.Type] = true
		case <-deadline:
			t.Fatalf("missing live updates: %v", seen)
		}
	}
	cancelRun()
	<-done
	history, err := rt.Events.History(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range history {
		if event.Type == "queue.updated" || event.Type == "diagnostics.updated" {
			t.Fatalf("UI-only update saved an event row: %+v", event)
		}
	}
}
