package scheduler_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runtimeauth"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// signAPIRequest proves the workspace secret for one runtime API request,
// exactly like the CLI does.
func signAPIRequest(t *testing.T, root string, request *http.Request) {
	t.Helper()
	secret, err := runtimeauth.LoadSecret(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtimeauth.SignRequest(request, secret, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// apiRequest builds a signed request for one runtime API endpoint.
func apiRequest(t *testing.T, root, method, target string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	signAPIRequest(t, root, request)
	return request
}

// dashboardSession exchanges a fresh one-time dashboard link for the owner
// session cookie a browser would hold.
func dashboardSession(t *testing.T, server *web.Server) *http.Cookie {
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

// dashboardRequest builds a browser request that carries the owner session
// cookie, like a user who followed the link printed by mergeyard start.
func dashboardRequest(t *testing.T, server *web.Server, method, target string, body io.Reader) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, body)
	request.AddCookie(dashboardSession(t, server))
	return request
}

// runWorktree returns the managed worktree the runtime prepared for one run.
func runWorktree(t *testing.T, root string, run workflow.Run) string {
	t.Helper()
	path, err := managedgit.WorktreePath(root, run.Repository, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// manualCommand returns the runtime's interactive resume after takeover
// prepared the run; the CLI must rebuild the same invocation from the run
// snapshot and its own configuration.
func manualCommand(t *testing.T, s *scheduler.Scheduler, id string) harness.InteractiveCommand {
	t.Helper()
	command, err := s.ManualCommand(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return command
}
