package scheduler_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestAgentPortExhaustionKeepsRetryFailedAndReconcilesStop(t *testing.T) {
	t.Parallel()

	flow := testkit.NewScenario(t)
	_, runtime, api, _, cfg, r := flow.Parts()
	cfg.AgentPorts = config.AgentPorts{Base: 65534, Size: 2}
	s, err := flow.New(cfg, scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	run := flow.Until(s, workflow.WaitingForCI, workflow.Review)
	if _, err := runtime.Workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.InternalFailure, Failure: &fault.Error{Code: "test.failed", Message: "Test failure"}}); err != nil {
		t.Fatal(err)
	}
	api.Issues["owner/repo"] = append(api.Issues["owner/repo"], testkit.Ready(8), testkit.Ready(9))
	if _, err := runtime.Workflow.Transition(ctx, "holder", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 8, AgentPorts: &cfg.AgentPorts}); err != nil {
		t.Fatal(err)
	}
	selected, err := s.Retry(ctx, run.ID)
	var coded *fault.Error
	if !errors.As(err, &coded) || coded.Code != "agent_ports.exhausted" || selected.State != workflow.Failed || selected.PendingRetry() != nil {
		t.Fatalf("exhausted Retry: %+v %v", selected, err)
	}
	// An upgrade can also encounter an unassigned nonterminal run. It must not
	// prevent a durable stop from releasing the sole occupied block.
	if _, err := runtime.Workflow.Transition(ctx, "legacy", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 9}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Workflow.Transition(ctx, "legacy", workflow.Request{Trigger: workflow.TakeOver}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.DB.ExecContext(ctx, "UPDATE runs SET stop_requested=1 WHERE id='holder'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	holder, err := runtime.Workflow.Get(ctx, "holder")
	if err != nil || holder.State != workflow.Stopped || holder.AgentPorts != nil {
		t.Fatalf("stop did not release lease: %+v %v", holder, err)
	}
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	legacy, err := runtime.Workflow.Get(ctx, "legacy")
	if err != nil || legacy.AgentPorts == nil || legacy.AgentPorts.Base != 65534 {
		t.Fatalf("legacy run did not reuse released block: %+v %v", legacy.AgentPorts, err)
	}
}

func TestLegacyReviewWithoutAvailablePortsLeavesRestartUsable(t *testing.T) {
	t.Parallel()

	flow := testkit.NewScenario(t)
	_, runtime, api, _, cfg, r := flow.Parts()
	cfg.AgentPorts = config.AgentPorts{Base: 65534, Size: 2}
	deps := scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r}
	s, err := flow.New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	run := flow.Until(s, workflow.Active, workflow.Review)
	// Represent an upgraded run awaiting its first reviewer, without a lease.
	if _, err := runtime.DB.ExecContext(ctx, "DELETE FROM agent_port_leases WHERE run_id=?", run.ID); err != nil {
		t.Fatal(err)
	}
	api.Issues["owner/repo"] = append(api.Issues["owner/repo"], testkit.Ready(8))
	if _, err := runtime.Workflow.Transition(ctx, "holder", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 8, AgentPorts: &cfg.AgentPorts}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Workflow.Transition(ctx, "holder", workflow.Request{Trigger: workflow.TakeOver}); err != nil {
		t.Fatal(err)
	}
	s, err = flow.New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatalf("port exhaustion prevented restart: %v", err)
	}
	waiting, err := runtime.Workflow.Get(ctx, run.ID)
	if err != nil || waiting.State != workflow.NeedsAttention || waiting.LastErrorCode != "agent_ports.exhausted" || waiting.AgentPorts != nil {
		t.Fatalf("unassigned reviewer was not held safely: %+v %v", waiting, err)
	}
	if err := s.Stop(ctx, "holder"); err != nil {
		t.Fatal(err)
	}
	selected, err := s.Retry(ctx, run.ID)
	if err != nil || selected.AgentPorts == nil || selected.AgentPorts.Base != 65534 {
		t.Fatalf("Retry did not acquire released block: %+v %v", selected, err)
	}
	testkit.AdvanceFixUntil(t, s, runtime, run.ID, func(current workflow.Run) bool {
		return current.State == workflow.WaitingForCI && current.Phase == workflow.Review
	})
}
