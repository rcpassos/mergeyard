package scheduler_test

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
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestStopLiveImplementPreservesCodeAndUpdatesLabels(t *testing.T) {
	s, runtime, api, _, _, r := localFlow(t, `/bin/sleep 60`)
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
	if got := gitCommand(t, path, "branch", "--show-current"); got != "mergeyard/issue-7" {
		t.Fatalf("preserved branch = %s", got)
	}

	issue := api.issues["owner/repo"][0]
	if has(issue, "agent-running") || has(issue, "ready-for-agent") || !has(issue, "agent-needs-attention") {
		t.Fatalf("stop labels: %+v", issue.Labels)
	}
	if _, err := s.Watch(ctx, run.ID); err == nil {
		t.Fatal("stopped run can be watched")
	}
	if err := s.Stop(ctx, run.ID); err != nil {
		t.Fatalf("repeated stop: %v", err)
	}
}

func TestStopWithoutCreatedWorkDoesNotAddAttention(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}}
	s, runtime := configured(t, api, "repositories:\n  - repo: owner/repo\n", rejectingGit{})
	ctx := context.Background()
	if _, err := runtime.Workflow.Transition(ctx, "unprepared", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7}); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(ctx, "unprepared"); err != nil {
		t.Fatal(err)
	}
	run, _ := runtime.Workflow.Get(ctx, "unprepared")
	if run.State != workflow.Stopped || has(api.issues["owner/repo"][0], "agent-needs-attention") {
		t.Fatalf("unprepared stop: %+v labels %+v", run, api.issues)
	}
	// The next scheduling tick must not turn this stopped issue into a new run.
	tickErr := s.Tick(ctx)
	runs, err := s.Runs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != "unprepared" || runs[0].State != workflow.Stopped {
		t.Fatalf("early stop redispatched issue: %+v", runs)
	}
	if tickErr != nil {
		t.Fatal(tickErr)
	}
	if has(api.issues["owner/repo"][0], "ready-for-agent") {
		t.Fatal("stop left issue ready for redispatch")
	}
}

func TestStopIntentSurvivesLabelFailureAndRestart(t *testing.T) {
	s, runtime, api, _, cfg, r := localFlow(t, `/bin/sleep 60`)
	ctx := context.Background()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(ctx)
	id := runs[0].ID
	api.mutate = func(action, repo string, n int, label string) error { return errors.New("GitHub unavailable") }
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
	api.mutate = nil
	next, err := scheduler.New(cfg, schedulerResources(restarted), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Reconcile(ctx); err != nil {
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
	s, runtime, api, _, cfg, r := localFlow(t, `/bin/sleep 60`)
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
	next, err := scheduler.New(cfg, schedulerResources(restarted), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Reconcile(ctx); err != nil {
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
	s, runtime, api, _, _, _ := localFlow(t, `/bin/sleep 60`)
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
	server.ServeHTTP(statusResponse, httptest.NewRequest("GET", "http://127.0.0.1:7331/api/status", nil))
	var status web.Status
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Workspace != runtime.Workspace.Root || status.Token == "" || len(status.Runs) != 1 {
		t.Fatalf("status: %+v", status)
	}
	watch := httptest.NewRecorder()
	server.ServeHTTP(watch, httptest.NewRequest("GET", "http://127.0.0.1:7331/api/runs/"+id+"/watch", nil))
	if watch.Code != http.StatusOK {
		t.Fatalf("watch API: %d %s", watch.Code, watch.Body.String())
	}
	for _, authorized := range []bool{false, true} {
		request := httptest.NewRequest("POST", "http://127.0.0.1:7331/api/runs/"+id+"/stop", nil)
		if authorized {
			request.Header.Set("Origin", "http://127.0.0.1:7331")
			request.Header.Set("X-CSRF-Token", status.Token)
		}
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		run, _ := runtime.Workflow.Get(ctx, id)
		if !authorized && (response.Code != http.StatusForbidden || run.State != workflow.Active) {
			t.Fatalf("unauthorized stop changed run: %d %+v", response.Code, run)
		}
		if authorized && (response.Code != http.StatusNoContent || run.State != workflow.Stopped || has(api.issues["owner/repo"][0], "agent-running")) {
			t.Fatalf("authorized stop failed: %d %+v", response.Code, run)
		}
	}
}

func TestEarlyStopRetriesReadyRemovalBeforeBecomingTerminal(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}}
	const yaml = "repositories:\n  - repo: owner/repo\n"
	s, runtime := configured(t, api, yaml, rejectingGit{})
	ctx := context.Background()
	if _, err := runtime.Workflow.Transition(ctx, "early-stop", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7}); err != nil {
		t.Fatal(err)
	}
	readyFailure := errors.New("ready removal unavailable")
	api.mutate = func(action, repo string, n int, label string) error {
		if action == "remove" && label == "ready-for-agent" {
			return readyFailure
		}
		return nil
	}
	if err := s.Stop(ctx, "early-stop"); !errors.Is(err, readyFailure) {
		t.Fatalf("ready removal failure was hidden: %v", err)
	}
	run, err := runtime.Workflow.Get(ctx, "early-stop")
	if err != nil || run.State != workflow.Claiming {
		t.Fatalf("failed stop became terminal: %+v %v", run, err)
	}
	if err := s.Tick(ctx); !errors.Is(err, readyFailure) {
		t.Fatalf("tick did not retry pending stop: %v", err)
	}
	runs, err := s.Runs(ctx)
	if err != nil || len(runs) != 1 {
		t.Fatalf("failed stop allowed redispatch: %+v %v", runs, err)
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
	cfg, _, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	next, err := scheduler.New(cfg, schedulerResources(restarted), scheduler.Dependencies{GitHub: api, Git: rejectingGit{}})
	if err != nil {
		t.Fatal(err)
	}
	api.mutate = nil
	for range 2 {
		if err := next.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	runs, err = next.Runs(ctx)
	if err != nil || len(runs) != 1 || runs[0].State != workflow.Stopped {
		t.Fatalf("restart lost stop or redispatched: %+v %v", runs, err)
	}
	issue := api.issues["owner/repo"][0]
	if has(issue, "ready-for-agent") || has(issue, "agent-running") || has(issue, "agent-needs-attention") {
		t.Fatalf("early stop labels: %+v", issue.Labels)
	}
}
