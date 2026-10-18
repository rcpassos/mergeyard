package web_test

import (
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
	"time"

	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/runtimeauth"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/store"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
	webassets "github.com/rcpassos/mergeyard/web"
)

type stubOperations struct{ runs []workflow.Run }

func (s stubOperations) Runs(context.Context) ([]workflow.Run, error) { return s.runs, nil }
func (stubOperations) Stop(context.Context, string) error             { return nil }
func (stubOperations) Watch(context.Context, string) (runner.SessionRef, error) {
	return runner.SessionRef{Name: "phase"}, nil
}
func (stubOperations) Takeover(context.Context, string) (workflow.Run, error) {
	return workflow.Run{}, nil
}
func (stubOperations) Handback(context.Context, string) (workflow.Run, error) {
	return workflow.Run{}, nil
}
func (stubOperations) Retry(context.Context, string, ...string) (workflow.Run, error) {
	return workflow.Run{}, nil
}

func newAPIServer(t *testing.T) (*web.Server, *httptest.Server, []byte, func() bool) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	bus := events.New(db, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	t.Cleanup(bus.Close)
	control := scheduler.NewControl(bus)
	root := t.TempDir()
	server, err := web.NewWithOperations(bus, control, stubOperations{runs: []workflow.Run{{ID: "run-1", Repository: "owner/repo", IssueNumber: 7, State: workflow.Active}}}, root)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := runtimeauth.LoadSecret(root)
	if err != nil {
		t.Fatal(err)
	}
	return server, httptest.NewServer(server), secret, control.Paused
}

// ownerSession follows a one-time dashboard link and returns the session
// cookie a browser would hold.
func ownerSession(t *testing.T, server *web.Server) *http.Cookie {
	t.Helper()
	exchange := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/?token="+server.NewDashboardToken(), nil)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, exchange)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("dashboard link: %d %s", recorder.Code, recorder.Body)
	}
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == "mergeyard_dashboard" {
			return cookie
		}
	}
	t.Fatal("dashboard link did not set the owner session")
	return nil
}

func TestAPIRequestsRequireTheWorkspaceSecret(t *testing.T) {
	_, server, secret, _ := newAPIServer(t)
	defer server.Close()

	// An unauthenticated caller learns nothing, not even the CSRF token.
	response, err := http.Get(server.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || response.Header.Get("X-Mergeyard-Error-Code") != "runtime.unauthenticated" {
		t.Fatalf("unsigned status: %d %q", response.StatusCode, response.Header.Get("X-Mergeyard-Error-Code"))
	}
	if strings.Contains(string(body), "csrf_token") {
		t.Fatalf("unauthenticated response leaked the CSRF token: %s", body)
	}

	// A proven caller receives the snapshot and the runtime proves itself back.
	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtimeauth.SignRequest(request, secret, time.Now()); err != nil {
		t.Fatal(err)
	}
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("signed status: %d %v", response.StatusCode, err)
	}
	if err := runtimeauth.VerifyResponse(response, secret, body); err != nil {
		t.Fatalf("runtime did not prove itself: %v", err)
	}
	var status web.Status
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatal(err)
	}
	if status.Token == "" || len(status.Runs) != 1 {
		t.Fatalf("status: %+v", status)
	}

	// API controls need the proof even with an origin and token.
	response, err = http.Post(server.URL+"/api/runs/run-1/stop", "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unproven stop: %d", response.StatusCode)
	}
}

func TestDashboardIsLockedToTheOwnerSession(t *testing.T) {
	server, httpServer, _, paused := newAPIServer(t)
	defer httpServer.Close()
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	// A local process that never followed the printed link sees a locked page
	// without the CSRF token, and cannot mutate anything.
	page, err := client.Get(httpServer.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if page.StatusCode != http.StatusOK || !strings.Contains(string(body), "Dashboard locked") {
		t.Fatalf("locked page: %d %s", page.StatusCode, body)
	}
	for _, forbidden := range []string{"csrf_token", "Pause scheduler", "Resume scheduler"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("locked page exposed %q: %s", forbidden, body)
		}
	}
	attack, err := http.NewRequest(http.MethodPost, httpServer.URL+"/scheduler/pause", strings.NewReader("csrf_token=stolen"))
	if err != nil {
		t.Fatal(err)
	}
	attack.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	attack.Header.Set("Origin", httpServer.URL)
	response, err := client.Do(attack)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden || paused() {
		t.Fatalf("locked dashboard accepted a control: %d paused=%v", response.StatusCode, paused())
	}

	// Following a one-time link sets the owner session; the page then renders
	// the CSRF token and controls work.
	cookie := ownerSession(t, server)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Value == "" {
		t.Fatalf("owner session cookie: %+v", cookie)
	}
	pageRequest, err := http.NewRequest(http.MethodGet, httpServer.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	pageRequest.AddCookie(cookie)
	page, err = client.Do(pageRequest)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(page.Body)
	page.Body.Close()
	token := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`).FindStringSubmatch(string(body))
	if page.StatusCode != http.StatusOK || len(token) != 2 || !strings.Contains(string(body), "Pause scheduler") {
		t.Fatalf("owner page: %d %s", page.StatusCode, body)
	}
	form := url.Values{"csrf_token": {token[1]}}
	request, err := http.NewRequest(http.MethodPost, httpServer.URL+"/scheduler/pause", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", httpServer.URL)
	request.AddCookie(cookie)
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || !paused() {
		t.Fatalf("owner pause: %d paused=%v", response.StatusCode, paused())
	}

	// Links are single-use: replaying one is refused.
	link := server.NewDashboardToken()
	first := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/runs/run-1?token="+link, nil)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, first)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/runs/run-1" {
		t.Fatalf("link exchange: %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
	replay := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/runs/run-1?token="+link, nil)
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, replay)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("replayed link accepted: %d", recorder.Code)
	}
}

func TestDashboardAssetsNeverSendTheWorkspaceSecret(t *testing.T) {
	// The embedded dashboard script must not learn or forward the secret.
	data, err := webassets.Files.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{runtimeauth.NonceHeader, runtimeauth.ProofHeader, "X-Mergeyard-Secret"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("dashboard script references %s", forbidden)
		}
	}
}
