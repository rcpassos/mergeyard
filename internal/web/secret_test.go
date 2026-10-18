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

func newAPIServer(t *testing.T) (*httptest.Server, []byte, func() bool) {
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
	return httptest.NewServer(server), secret, control.Paused
}

func TestAPIRequestsRequireTheWorkspaceSecret(t *testing.T) {
	server, secret, _ := newAPIServer(t)
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
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("signed status: %d", response.StatusCode)
	}
	if err := runtimeauth.VerifyResponse(response, secret); err != nil {
		t.Fatalf("runtime did not prove itself: %v", err)
	}
	var status web.Status
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
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

func TestDashboardKeepsWorkingWithoutTheSecret(t *testing.T) {
	server, _, paused := newAPIServer(t)
	defer server.Close()

	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	page, err := client.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if page.StatusCode != http.StatusOK {
		t.Fatalf("dashboard page: %d", page.StatusCode)
	}
	token := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`).FindStringSubmatch(string(body))
	if len(token) != 2 {
		t.Fatal("dashboard did not supply a CSRF token")
	}

	form := url.Values{"csrf_token": {token[1]}}
	request, err := http.NewRequest(http.MethodPost, server.URL+"/scheduler/pause", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", server.URL)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || !paused() {
		t.Fatalf("browser pause: %d paused=%v", response.StatusCode, paused())
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
