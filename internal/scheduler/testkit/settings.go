package testkit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/web"
)

// settingsPage starts the dashboard updates for one server and returns the
// rendered Settings page once the doctor report has arrived.
func SettingsPage(t *testing.T, server *web.Server, ready string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); server.RunUpdates(ctx) }()
	defer func() { cancel(); <-done }()
	var body string
	WaitForWithin(t, 10*time.Second, func() bool {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/settings", nil))
		body = response.Body.String()
		return response.Code == http.StatusOK && strings.Contains(body, ready)
	})
	return body
}
