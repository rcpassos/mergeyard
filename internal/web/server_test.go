package web_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/store"
	"github.com/rcpassos/mergeyard/internal/web"
)

func TestSchedulerDatabaseFailurePreservesSafeDiagnostics(t *testing.T) {
	for _, action := range []string{"pause", "resume"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			bus := events.New(db, slog.New(slog.NewJSONHandler(io.Discard, nil)))
			t.Cleanup(bus.Close)
			control := scheduler.NewControl(bus)
			if action == "resume" {
				if err := control.Pause(ctx); err != nil {
					t.Fatal(err)
				}
			}
			server, err := web.New(bus, control)
			if err != nil {
				t.Fatal(err)
			}
			page := httptest.NewRecorder()
			server.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/", nil))
			token := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`).FindStringSubmatch(page.Body.String())
			if len(token) != 2 {
				t.Fatal("dashboard did not supply a CSRF token")
			}
			// Fail real event storage after the browser has obtained its token.
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			path := "/scheduler/" + action
			form := url.Values{"csrf_token": {token[1]}}
			request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7331"+path, strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.Header.Set("Origin", "http://127.0.0.1:7331")
			request.Header.Set("HX-Request", "true")
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", response.Code)
			}
			if code := response.Header().Get("X-Mergeyard-Error-Code"); code != "internal.event_store" {
				t.Errorf("diagnostic code = %q, want internal.event_store", code)
			}
			if !strings.Contains(response.Body.String(), "internal.event_store") {
				t.Errorf("response lost diagnostic code: %q", response.Body.String())
			}
			if strings.Contains(response.Body.String(), "sql: database is closed") {
				t.Error("response exposed the database failure cause")
			}
			var entry struct {
				Code   string `json:"error_code"`
				Action string `json:"action"`
				Error  string `json:"error"`
			}
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
				t.Fatalf("missing structured failure log: %v; log = %q", err, logs.String())
			}
			if entry.Code != "internal.event_store" || entry.Action != path || !strings.Contains(entry.Error, "sql: database is closed") {
				t.Errorf("failure log lost diagnostics: %+v", entry)
			}
		})
	}
}
