package scheduler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runtimeauth"
	"github.com/rcpassos/mergeyard/internal/scheduler"
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

// runWorktree returns the managed worktree the runtime prepared for one run.
func runWorktree(t *testing.T, root string, run workflow.Run) string {
	t.Helper()
	path, err := managedgit.WorktreePath(root, run.Repository, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// manualCommand returns the interactive resume the CLI rebuilds locally after
// takeover prepared the run.
func manualCommand(t *testing.T, s *scheduler.Scheduler, id string) harness.InteractiveCommand {
	t.Helper()
	command, err := s.ManualCommand(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return command
}
