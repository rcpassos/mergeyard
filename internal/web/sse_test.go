package web_test

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/store"
	"github.com/rcpassos/mergeyard/internal/web"
)

func openStream(t *testing.T, url, lastEventID string) (*http.Response, *bufio.Reader) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	if lastEventID != "" {
		request.Header.Set("Last-Event-ID", lastEventID)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response, bufio.NewReader(response.Body)
}

func readFrame(t *testing.T, reader *bufio.Reader) []string {
	t.Helper()
	type result struct {
		lines []string
		err   error
	}
	frames := make(chan result, 1)
	go func() {
		var lines []string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				frames <- result{lines, err}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				frames <- result{lines, nil}
				return
			}
			lines = append(lines, line)
		}
	}()
	select {
	case got := <-frames:
		if got.err != nil {
			t.Fatalf("reading SSE frame: %v", got.err)
		}
		return got.lines
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an SSE frame")
		return nil
	}
}

func TestTransientNotificationRefreshesLiveStreamsWithoutJoiningHistory(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	bus := events.New(db, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	t.Cleanup(bus.Close)
	server, err := web.New(bus, scheduler.NewControl(bus))
	if err != nil {
		t.Fatal(err)
	}
	listener := httptest.NewServer(server)
	t.Cleanup(listener.Close)
	response, reader := openStream(t, listener.URL, "")
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream handshake: %d %q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	if frame := readFrame(t, reader); len(frame) != 1 || frame[0] != ": connected" {
		t.Fatalf("stream did not start connected: %v", frame)
	}
	if _, err := bus.Publish(ctx, events.Draft{Type: "scheduler.paused", Payload: map[string]bool{"paused": true}}); err != nil {
		t.Fatal(err)
	}
	first := readFrame(t, reader)
	if len(first) != 3 || first[0] != "id: 1" || first[1] != "event: scheduler.paused" || !strings.Contains(first[2], `"created_at"`) {
		t.Fatalf("durable frame = %v", first)
	}
	// A queue refresh is UI-only: subscribers see it live, with no ID line to
	// move the browser's replay cursor.
	if err := bus.Notify(ctx, events.Draft{Type: "queue.updated", Payload: struct{}{}}); err != nil {
		t.Fatal(err)
	}
	transient := readFrame(t, reader)
	for _, line := range transient {
		if strings.HasPrefix(line, "id:") {
			t.Fatalf("transient frame moved the replay cursor: %v", transient)
		}
	}
	if len(transient) != 2 || transient[0] != "event: queue.updated" || !strings.Contains(transient[1], `"type":"queue.updated"`) || strings.Contains(transient[1], `"id"`) {
		t.Fatalf("transient frame = %v", transient)
	}
	if _, err := bus.Publish(ctx, events.Draft{Type: "scheduler.resumed", Payload: map[string]bool{"paused": false}}); err != nil {
		t.Fatal(err)
	}
	second := readFrame(t, reader)
	if len(second) != 3 || second[0] != "id: 2" {
		t.Fatalf("transient notification reset the durable cursor: %v", second)
	}
	response.Body.Close()
	reconnect, replayed := openStream(t, listener.URL, "1")
	if reconnect.StatusCode != http.StatusOK {
		t.Fatalf("reconnect status = %d", reconnect.StatusCode)
	}
	if frame := readFrame(t, replayed); len(frame) != 1 {
		t.Fatalf("reconnect did not start connected: %v", frame)
	}
	frame := readFrame(t, replayed)
	if len(frame) != 3 || frame[0] != "id: 2" || frame[1] != "event: scheduler.resumed" {
		t.Fatalf("replay = %v", frame)
	}
	if _, err := bus.Publish(ctx, events.Draft{Type: "scheduler.paused", Payload: map[string]bool{"paused": true}}); err != nil {
		t.Fatal(err)
	}
	if frame := readFrame(t, replayed); len(frame) != 3 || frame[0] != "id: 3" {
		t.Fatalf("live frame after replay = %v", frame)
	}
}
