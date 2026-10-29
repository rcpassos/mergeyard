package scheduler_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestStopWithoutCreatedWorkDoesNotAddAttention(t *testing.T) {
	t.Parallel()

	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}}
	s, runtime := testkit.Configured(t, api, "repositories:\n  - repo: owner/repo\n", testkit.RejectingGit{})
	ctx := context.Background()
	if _, err := runtime.Workflow.Transition(ctx, "unprepared", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7}); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(ctx, "unprepared"); err != nil {
		t.Fatal(err)
	}
	run, _ := runtime.Workflow.Get(ctx, "unprepared")
	if run.State != workflow.Stopped || testkit.Has(api.Issues["owner/repo"][0], "agent-needs-attention") {
		t.Fatalf("unprepared stop: %+v labels %+v", run, api.Issues)
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
	if testkit.Has(api.Issues["owner/repo"][0], "ready-for-agent") {
		t.Fatal("stop left issue ready for redispatch")
	}
}

func TestEarlyStopRetriesReadyRemovalBeforeBecomingTerminal(t *testing.T) {
	t.Parallel()

	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}}
	const yaml = "repositories:\n  - repo: owner/repo\n"
	s, runtime := testkit.Configured(t, api, yaml, testkit.RejectingGit{})
	ctx := context.Background()
	if _, err := runtime.Workflow.Transition(ctx, "early-stop", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7}); err != nil {
		t.Fatal(err)
	}
	readyFailure := errors.New("ready removal unavailable")
	api.Mutate = func(action, repo string, n int, label string) error {
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
	next, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(restarted), scheduler.Dependencies{GitHub: api, Git: testkit.RejectingGit{}})
	if err != nil {
		t.Fatal(err)
	}
	api.Mutate = nil
	for range 2 {
		if err := next.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	runs, err = next.Runs(ctx)
	if err != nil || len(runs) != 1 || runs[0].State != workflow.Stopped {
		t.Fatalf("restart lost stop or redispatched: %+v %v", runs, err)
	}
	issue := api.Issues["owner/repo"][0]
	if testkit.Has(issue, "ready-for-agent") || testkit.Has(issue, "agent-running") || testkit.Has(issue, "agent-needs-attention") {
		t.Fatalf("early stop labels: %+v", issue.Labels)
	}
}
