package events_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/store"
)

func newBus(t *testing.T) (*events.Bus, *sql.DB, *bytes.Buffer) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	logs := new(bytes.Buffer)
	bus := events.New(db, slog.New(slog.NewJSONHandler(logs, nil)))
	return bus, db, logs
}

func TestPublishReplayAndSlowSubscriber(t *testing.T) {
	ctx := context.Background()
	bus, _, _ := newBus(t)
	slow, cancelSlow := bus.Subscribe(1)
	defer cancelSlow()
	fast, cancelFast := bus.Subscribe(3)
	defer cancelFast()
	first, err := bus.Publish(ctx, events.Draft{Type: "scheduler.paused", Payload: map[string]bool{"paused": true}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := bus.Publish(ctx, events.Draft{Type: "scheduler.resumed", Payload: map[string]bool{"paused": false}})
	if err != nil {
		t.Fatal(err)
	}
	if got := <-slow; got.ID != first.ID {
		t.Fatalf("slow consumer lost buffered event: %+v", got)
	}
	if _, open := <-slow; open {
		t.Fatal("overflowed subscription must close so the consumer can replay")
	}
	if got := <-fast; got.ID != first.ID {
		t.Fatalf("first live event = %+v", got)
	}
	if got := <-fast; got.ID != second.ID {
		t.Fatalf("second live event = %+v", got)
	}
	replay, err := bus.History(ctx, first.ID, 1)
	if err != nil || len(replay) != 1 || replay[0].ID != second.ID || replay[0].Type != "scheduler.resumed" || string(replay[0].Payload) != `{"paused":false}` || replay[0].RunID != "" {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	cancelFast()
	cancelFast()
	if _, open := <-fast; open {
		t.Fatal("cancel did not close subscription")
	}
}

func TestFailedEventsAreNeitherPersistedLoggedNorBroadcast(t *testing.T) {
	ctx := context.Background()
	bus, _, logs := newBus(t)
	live, cancel := bus.Subscribe(10)
	defer cancel()
	for _, draft := range []events.Draft{
		{Type: "phase.started", RunID: "missing", Payload: map[string]string{"phase": "implement"}},
		{Type: "scheduler.paused", Payload: make(chan int)},
		{Payload: "missing event type"},
	} {
		if _, err := bus.Publish(ctx, draft); err == nil {
			t.Fatalf("invalid event was accepted: %+v", draft)
		}
	}
	history, err := bus.History(ctx, 0, 100)
	if err != nil || len(history) != 0 || logs.Len() != 0 {
		t.Fatalf("failed events escaped: history = %+v, logs = %s, error = %v", history, logs, err)
	}
	select {
	case e := <-live:
		t.Fatalf("failed event delivered: %+v", e)
	default:
	}
}

func TestSubscribersOwnTheirPayload(t *testing.T) {
	bus, _, _ := newBus(t)
	first, cancelFirst := bus.Subscribe(1)
	defer cancelFirst()
	second, cancelSecond := bus.Subscribe(1)
	defer cancelSecond()
	published, err := bus.Publish(context.Background(), events.Draft{Type: "scheduler.paused", Payload: map[string]bool{"paused": true}})
	if err != nil {
		t.Fatal(err)
	}
	consumed := <-first
	consumed.Payload[0] = '['
	if other := <-second; string(other.Payload) != `{"paused":true}` || string(published.Payload) != `{"paused":true}` {
		t.Fatal("a consumer mutated another consumer's event")
	}
}

func TestCloseEndsStreamsAndRejectsWrites(t *testing.T) {
	bus, _, _ := newBus(t)
	live, cancel := bus.Subscribe(1)
	defer cancel()
	bus.Close()
	bus.Close()
	if _, open := <-live; open {
		t.Fatal("shutdown did not close subscription")
	}
	after, cancelAfter := bus.Subscribe(1)
	defer cancelAfter()
	if _, open := <-after; open {
		t.Fatal("closed bus allowed new live subscription")
	}
	_, err := bus.Publish(context.Background(), events.Draft{Type: "scheduler.paused", Payload: nil})
	var failure *fault.Error
	if !errors.As(err, &failure) || failure.Code != "internal.event_closed" {
		t.Fatalf("expected closed event bus error, got %v", err)
	}
	history, err := bus.History(context.Background(), 0, 100)
	if err != nil || len(history) != 0 {
		t.Fatalf("publish after close persisted an event: %+v, %v", history, err)
	}
}
