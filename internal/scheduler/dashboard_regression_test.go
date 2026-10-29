package scheduler_test

import (
	"context"
	"testing"

	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestStoppedClaimDoesNotRedispatch(t *testing.T) {
	t.Parallel()

	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}}
	s, runtime := testkit.Configured(t, api, "repositories:\n  - repo: owner/repo\n", testkit.RejectingGit{})
	ctx := context.Background()
	if _, err := runtime.Workflow.Transition(ctx, "interrupted-claim", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7}); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(ctx, "interrupted-claim"); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != "interrupted-claim" || runs[0].State != workflow.Stopped {
		t.Fatalf("stopped claim was redispatched: %+v", runs)
	}
}

func TestRepeatedStopDoesNotChangeNewerRunLabels(t *testing.T) {
	t.Parallel()

	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}}
	s, runtime := testkit.Configured(t, api, "repositories:\n  - repo: owner/repo\n", testkit.RejectingGit{})
	ctx := context.Background()
	claim := workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7}
	if _, err := runtime.Workflow.Transition(ctx, "old-run", claim); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(ctx, "old-run"); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Workflow.Transition(ctx, "new-run", claim); err != nil {
		t.Fatal(err)
	}
	if err := api.AddLabel(ctx, "owner/repo", 7, "agent-running"); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(ctx, "old-run"); err != nil {
		t.Fatal(err)
	}
	if !testkit.Has(api.Issues["owner/repo"][0], "agent-running") {
		t.Fatal("repeated Stop removed the newer run's running label")
	}
}
