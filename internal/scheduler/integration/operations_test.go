package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestStopLiveImplementPreservesCodeAndUpdatesLabels(t *testing.T) {
	t.Parallel()

	s, runtime, api, _, _, r := testkit.TmuxFlow(t, `/bin/sleep 60`)
	ctx := context.Background()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(ctx)
	run := runs[0]
	ref, err := s.Watch(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	stopped, err := runtime.Workflow.Get(ctx, run.ID)
	if err != nil || stopped.State != workflow.Stopped {
		t.Fatalf("stop: %+v %v", stopped, err)
	}
	status, err := r.SessionStatus(ctx, ref)
	if err != nil || status.State == runner.SessionRunning {
		t.Fatalf("phase still running: %+v %v", status, err)
	}
	var path string
	if err := runtime.DB.QueryRow("SELECT worktree_path FROM runs WHERE id=?", run.ID).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stop deleted worktree: %v", err)
	}
	if got := testkit.GitCommand(t, path, "branch", "--show-current"); got != "mergeyard/issue-7" {
		t.Fatalf("preserved branch = %s", got)
	}

	issue := api.Issues["owner/repo"][0]
	if testkit.Has(issue, "agent-running") || testkit.Has(issue, "ready-for-agent") || !testkit.Has(issue, "agent-needs-attention") {
		t.Fatalf("stop labels: %+v", issue.Labels)
	}
	if _, err := s.Watch(ctx, run.ID); err == nil {
		t.Fatal("stopped run can be watched")
	}
	if err := s.Stop(ctx, run.ID); err != nil {
		t.Fatalf("repeated stop: %v", err)
	}
}

func TestStopIntentSurvivesLabelFailureAndRestart(t *testing.T) {
	t.Parallel()

	s, runtime, api, _, cfg, r := testkit.TmuxFlow(t, `/bin/sleep 60`)
	ctx := context.Background()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(ctx)
	id := runs[0].ID
	api.Mutate = func(action, repo string, n int, label string) error { return errors.New("GitHub unavailable") }
	if err := s.Stop(ctx, id); err == nil {
		t.Fatal("label failure was hidden")
	}
	root := runtime.Workspace.Root
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := app.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	api.Mutate = nil
	// Reconciliation must finish the stop even if the issue closed while offline.
	api.Issues["owner/repo"][0].State = github.Closed
	next, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(restarted), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := next.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	run, _ := restarted.Workflow.Get(ctx, id)
	if run.State != workflow.Stopped {
		t.Fatalf("stop intent lost: %+v", run)
	}
	attempts, _ := filepath.Glob(filepath.Join(root, "runs", id, "phases", "*"))
	if len(attempts) != 1 {
		t.Fatalf("stop relaunched work: %v", attempts)
	}
}

func TestShutdownKeepsLiveImplementAndReconcileReusesIt(t *testing.T) {
	t.Parallel()

	s, runtime, api, _, cfg, r := testkit.TmuxFlow(t, `/bin/sleep 60`)
	ctx := context.Background()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(ctx)
	id := runs[0].ID
	ref, err := s.Watch(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	root := runtime.Workspace.Root
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	status, err := r.SessionStatus(ctx, ref)
	if err != nil || status.State != runner.SessionRunning {
		t.Fatalf("shutdown killed phase: %+v %v", status, err)
	}
	restarted, err := app.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	next, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(restarted), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := next.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	watched, err := next.Watch(ctx, id)
	if err != nil || watched != ref {
		t.Fatalf("restart did not pick up live phase: %+v %v", watched, err)
	}
	attempts, _ := filepath.Glob(filepath.Join(root, "runs", id, "phases", "*"))
	if len(attempts) != 1 {
		t.Fatalf("restart launched duplicate: %v", attempts)
	}
}

func TestLocalAPISharesControlsAndRequiresCSRFForStop(t *testing.T) {
	t.Parallel()

	s, runtime, api, _, _, _ := testkit.TmuxFlow(t, `/bin/sleep 60`)
	ctx := context.Background()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(ctx)
	id := runs[0].ID
	server, err := web.NewWithOperations(runtime.Events, runtime.Scheduler, s, runtime.Workspace.Root)
	if err != nil {
		t.Fatal(err)
	}
	statusResponse := httptest.NewRecorder()
	server.ServeHTTP(statusResponse, testkit.ApiRequest(t, runtime.Workspace.Root, "GET", "http://127.0.0.1:7331/api/status"))
	var status web.Status
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Workspace != runtime.Workspace.Root || status.Token == "" || len(status.Runs) != 1 {
		t.Fatalf("status: %+v", status)
	}
	watch := httptest.NewRecorder()
	server.ServeHTTP(watch, testkit.ApiRequest(t, runtime.Workspace.Root, "GET", "http://127.0.0.1:7331/api/runs/"+id+"/watch"))
	if watch.Code != http.StatusOK {
		t.Fatalf("watch API: %d %s", watch.Code, watch.Body.String())
	}
	for _, attack := range []string{"missing proof", "missing token", ""} {
		request := httptest.NewRequest("POST", "http://127.0.0.1:7331/api/runs/"+id+"/stop", nil)
		request.Header.Set("Origin", "http://127.0.0.1:7331")
		if attack != "missing proof" {
			testkit.SignAPIRequest(t, runtime.Workspace.Root, request)
		}
		if attack != "missing token" {
			request.Header.Set("X-CSRF-Token", status.Token)
		}
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		run, _ := runtime.Workflow.Get(ctx, id)
		if attack == "missing proof" && (response.Code != http.StatusUnauthorized || run.State != workflow.Active) {
			t.Fatalf("unproven stop changed run: %d %+v", response.Code, run)
		}
		if attack == "missing token" && (response.Code != http.StatusForbidden || run.State != workflow.Active) {
			t.Fatalf("unauthorized stop changed run: %d %+v", response.Code, run)
		}
		if attack == "" && (response.Code != http.StatusNoContent || run.State != workflow.Stopped || testkit.Has(api.Issues["owner/repo"][0], "agent-running")) {
			t.Fatalf("authorized stop failed: %d %+v", response.Code, run)
		}
	}
}
