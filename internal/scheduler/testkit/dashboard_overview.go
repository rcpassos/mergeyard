package testkit

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/web"
)

var (
	KpiValuePattern    = regexp.MustCompile(`id="kpi-([a-z]+)">([^<]+)<`)
	GroupCountPattern  = regexp.MustCompile(`id="group-([a-z]+)"[^>]*>[^<]*<span[^>]*>(\d+)</span>`)
	DashboardURLPrefix = "http://127.0.0.1:7331"
)

// dashboardBody serves one dashboard page for the overview tests.
func DashboardBody(t *testing.T, server *web.Server, path string) string {
	t.Helper()
	page := httptest.NewRecorder()
	server.ServeHTTP(page, DashboardRequest(t, server, http.MethodGet, DashboardURLPrefix+path, nil))
	if page.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", path, page.Code, page.Body.String())
	}
	return page.Body.String()
}

func Kpis(t *testing.T, body string) map[string]string {
	t.Helper()
	values := map[string]string{}
	for _, match := range KpiValuePattern.FindAllStringSubmatch(body, -1) {
		values[match[1]] = strings.TrimSpace(match[2])
	}
	return values
}

func QueueGroupCounts(t *testing.T, body string) map[string]string {
	t.Helper()
	counts := map[string]string{}
	for _, match := range GroupCountPattern.FindAllStringSubmatch(body, -1) {
		counts[match[1]] = match[2]
	}
	return counts
}

// section returns the markup from start to the next end marker.
func Section(t *testing.T, body, start, end string) string {
	t.Helper()
	at := strings.Index(body, start)
	if at < 0 {
		t.Fatalf("page lacks %q", start)
	}
	body = body[at:]
	if next := strings.Index(body, end); next >= 0 {
		body = body[:next]
	}
	return body
}
