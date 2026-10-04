package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/rcpassos/mergeyard/internal/events"
)

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	var cursor int64
	if value := r.Header.Get("Last-Event-ID"); value != "" {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id < 0 {
			http.Error(w, "Invalid event cursor", http.StatusBadRequest)
			return
		}
		cursor = id
	}
	// Subscribe first so commits during history replay cannot fall into a gap.
	live, cancel := s.bus.Subscribe(256)
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	write := func(frame string) error {
		if err := controller.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
			return err
		}
		if _, err := fmt.Fprint(w, frame); err != nil {
			return err
		}
		return controller.Flush()
	}
	if err := write(": connected\n\n"); err != nil {
		return
	}
	emit := func(event events.Event) error {
		if event.ID <= cursor {
			return nil
		}
		data, err := json.Marshal(event)
		if err != nil {
			return err
		}
		// The event type is persisted data, so JSON is the safe fallback if a
		// producer supplied characters that could change SSE framing.
		name := event.Type
		for _, ch := range name {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '.' || ch == '_' || ch == '-') {
				name = "message"
				break
			}
		}
		if err := write(fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", event.ID, name, data)); err != nil {
			return err
		}
		cursor = event.ID
		return nil
	}
	// A new connection starts live. Reconnects replay durable history in
	// bounded pages and deduplicate the overlapping subscription by event ID.
	if r.Header.Get("Last-Event-ID") != "" {
		for {
			history, err := s.bus.History(r.Context(), cursor, 1000)
			if err != nil {
				return
			}
			for _, event := range history {
				if err := emit(event); err != nil {
					return
				}
			}
			if len(history) < 1000 {
				break
			}
		}
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-live:
			if !ok || emit(event) != nil {
				return
			}
		case <-heartbeat.C:
			if err := write(": heartbeat\n\n"); err != nil {
				return
			}
		}
	}
}
