package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDashboardFollowsTheSystemThemeAndServesItsFavicon(t *testing.T) {
	server, httpServer, _, _ := newAPIServer(t)
	defer httpServer.Close()
	session := ownerSession(t, server, httpServer.URL)
	for _, path := range []string{"/", "/queue", "/settings"} {
		body := dashboardGET(t, httpServer.Client(), httpServer.URL+path, session)
		// A fixed theme on the root would defeat the prefers-color-scheme switch.
		if strings.Contains(body, "data-theme") {
			t.Errorf("%s pins a theme: %s", path, body)
		}
		if !strings.Contains(body, `<link rel="icon" href="/static/favicon.svg" type="image/svg+xml">`) {
			t.Errorf("%s does not link the favicon: %s", path, body)
		}
	}

	// The favicon is public like every asset, so the locked tab shows it too.
	favicon := httptest.NewRecorder()
	server.ServeHTTP(favicon, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/static/favicon.svg", nil))
	if favicon.Code != http.StatusOK || !strings.HasPrefix(favicon.Header().Get("Content-Type"), "image/svg+xml") {
		t.Fatalf("favicon: %d %q", favicon.Code, favicon.Header().Get("Content-Type"))
	}
	if !strings.Contains(favicon.Body.String(), "prefers-color-scheme: dark") {
		t.Fatalf("favicon does not follow the color scheme: %s", favicon.Body)
	}
}
