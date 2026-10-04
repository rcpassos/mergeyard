package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/rcpassos/mergeyard/internal/fault"
)

// Event is a committed entry in the durable, globally ordered event stream.
type Event struct {
	ID        int64           `json:"id"`
	RunID     string          `json:"run_id,omitempty"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt string          `json:"created_at"`
}

// Draft describes an event before its ID and timestamp have been assigned.
// An empty RunID denotes an application event, such as scheduler.paused.
type Draft struct {
	RunID   string
	Type    string
	Payload any
}

// Bus writes events to SQLite and broadcasts them after commit. Use one Bus
// per runtime so commits and subscriber delivery share the same ordering.
type Bus struct {
	db          *sql.DB
	logger      *slog.Logger
	mu          sync.Mutex
	subscribers map[chan Event]struct{}
	closed      bool
}

func New(db *sql.DB, logger *slog.Logger) *Bus {
	if logger == nil {
		logger = slog.Default()
	}
	return &Bus{db: db, logger: logger, subscribers: make(map[chan Event]struct{})}
}

// Publish persists a standalone event. Run transitions use Commit instead.
func (b *Bus) Publish(ctx context.Context, draft Draft) (Event, error) {
	return b.Commit(ctx, func(*sql.Tx) (Draft, error) { return draft, nil })
}

// Commit atomically records an event with the mutation performed by change.
// The callback must use the supplied transaction and must not call the Bus.
// No log or subscriber notification escapes a rolled-back transaction.
func (b *Bus) Commit(ctx context.Context, change func(*sql.Tx) (Draft, error)) (Event, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return Event{}, &fault.Error{Code: "internal.event_closed", Message: "Event bus is closed"}
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, databaseError(err)
	}
	defer tx.Rollback()
	draft, err := change(tx)
	if err != nil {
		return Event{}, err
	}
	if draft.Type == "" {
		return Event{}, &fault.Error{Code: "internal.event_invalid", Message: "Event type is required"}
	}
	payload, err := json.Marshal(draft.Payload)
	if err != nil {
		return Event{}, &fault.Error{Code: "internal.event_invalid", Message: "Event payload must be valid JSON", Err: err}
	}
	var runID any
	if draft.RunID != "" {
		runID = draft.RunID
	}
	event := Event{RunID: draft.RunID, Type: draft.Type, Payload: payload}
	err = tx.QueryRowContext(ctx, `INSERT INTO events (run_id, type, payload_json)
		VALUES (?, ?, ?) RETURNING id, created_at`, runID, draft.Type, string(payload)).Scan(&event.ID, &event.CreatedAt)
	if err != nil {
		return Event{}, databaseError(err)
	}
	if err := tx.Commit(); err != nil {
		return Event{}, databaseError(err)
	}
	b.logger.InfoContext(ctx, "event", "event_id", event.ID, "run_id", event.RunID,
		"event_type", event.Type, "payload", event.Payload, "created_at", event.CreatedAt)
	for subscriber := range b.subscribers {
		copy := event
		copy.Payload = append(json.RawMessage(nil), event.Payload...)
		select {
		case subscriber <- copy:
		default:
			// Disconnect slow consumers; they can replay from their last ID.
			close(subscriber)
			delete(b.subscribers, subscriber)
		}
	}
	return event, nil
}

// Subscribe returns a bounded stream and an idempotent cancellation function.
// A full buffer closes the stream instead of blocking run progress or silently
// dropping events. Reconnect by subscribing before replaying History and
// deduplicating live events by ID. buffer must be at least one.
func (b *Bus) Subscribe(buffer int) (<-chan Event, func()) {
	if buffer < 1 {
		buffer = 1
	}
	ch := make(chan Event, buffer)
	b.mu.Lock()
	if b.closed {
		close(ch)
	} else {
		b.subscribers[ch] = struct{}{}
	}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subscribers[ch]; ok {
			delete(b.subscribers, ch)
			close(ch)
		}
	}
}

// Close waits for any current commit and closes all live streams. The runtime
// calls it before closing SQLite. It is safe to call more than once.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for ch := range b.subscribers {
		close(ch)
		delete(b.subscribers, ch)
	}
}

// History returns up to limit events after afterID, in durable ID order.
func (b *Bus) History(ctx context.Context, afterID int64, limit int) ([]Event, error) {
	if afterID < 0 || limit < 1 || limit > 1000 {
		return nil, &fault.Error{Code: "internal.event_invalid", Message: "History requires a nonnegative cursor and a limit from 1 to 1000"}
	}
	rows, err := b.db.QueryContext(ctx, `SELECT id, COALESCE(run_id, ''), type, payload_json, created_at
		FROM events WHERE id > ? ORDER BY id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, databaseError(err)
	}
	defer rows.Close()
	var result []Event
	for rows.Next() {
		var e Event
		var payload string
		if err := rows.Scan(&e.ID, &e.RunID, &e.Type, &payload, &e.CreatedAt); err != nil {
			return nil, databaseError(err)
		}
		e.Payload = json.RawMessage(payload)
		result = append(result, e)
	}
	if err := rows.Err(); err != nil {
		return nil, databaseError(err)
	}
	return result, nil
}

func databaseError(err error) error {
	return &fault.Error{Code: "internal.event_store", Message: "Could not persist or read run events", Err: fmt.Errorf("event storage: %w", err)}
}
