package scheduler_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestAgentPortEnvironmentReachesImplementReviewAndFixWrappers(t *testing.T) {
	script := `printf '%s\n' "$MERGEYARD_RUN_ID" "$MERGEYARD_PHASE" "$MERGEYARD_PORT_BASE" "$MERGEYARD_PORT_COUNT" "$EXPLICIT_VALUE" > "$TRACE_DIR/$MERGEYARD_PHASE"` + "\n" + loopScript(`printf fixed > feature.txt; `+fixedReport)
	_, runtime, api, _, cfg, r := localFlow(t, script)
	cfg.AgentPorts = config.AgentPorts{Base: 6100, Size: 11}
	trace := t.TempDir()
	env := map[string]string{"TRACE_DIR": trace, "EXPLICIT_VALUE": "kept", "MERGEYARD_PORT_BASE": "untrusted"}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	finish(t, s, workflow.Active, workflow.Review)
	// Rebuild the control plane before review and fix, keeping the stored range.
	cfg.AgentPorts = config.AgentPorts{Base: 6200, Size: 10}
	s, err = scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
	for _, tc := range []struct{ phase, base, count, dir string }{
		{"implement", "6100", "5", "implement-0-1"},
		{"review", "6105", "6", "review-2-1"},
		{"fix", "6100", "5", "fix-1-1"},
	} {
		data, err := os.ReadFile(filepath.Join(trace, tc.phase))
		want := strings.Join([]string{run.ID, tc.phase, tc.base, tc.count, "kept", ""}, "\n")
		if err != nil || string(data) != want {
			t.Fatalf("%s wrapper environment: %q, want %q: %v", tc.phase, data, want, err)
		}
		input, err := os.ReadFile(filepath.Join(runtime.Workspace.Root, "runs", run.ID, "phases", tc.dir, "input.md"))
		for _, name := range []string{"MERGEYARD_RUN_ID", "MERGEYARD_PHASE", "MERGEYARD_PORT_BASE", "MERGEYARD_PORT_COUNT", "AGENTS.md", "CLAUDE.md"} {
			if err != nil || !strings.Contains(string(input), name) {
				t.Fatalf("%s input missing isolation instruction %s: %v", tc.phase, name, err)
			}
		}
	}
	if env["MERGEYARD_PORT_BASE"] != "untrusted" {
		t.Fatal("scheduler changed caller environment")
	}
}

func TestAgentPortExhaustionKeepsRetryFailedAndReconcilesStop(t *testing.T) {
	_, runtime, api, _, cfg, r := localFlow(t, successfulScript)
	cfg.AgentPorts = config.AgentPorts{Base: 65534, Size: 2}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if _, err := runtime.Workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.InternalFailure, Failure: &fault.Error{Code: "test.failed", Message: "Test failure"}}); err != nil {
		t.Fatal(err)
	}
	api.issues["owner/repo"] = append(api.issues["owner/repo"], ready(8), ready(9))
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
	_, runtime, api, _, cfg, r := localFlow(t, successfulScript)
	cfg.AgentPorts = config.AgentPorts{Base: 65534, Size: 2}
	deps := scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r}
	s, err := scheduler.New(cfg, schedulerResources(runtime), deps)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	run := finish(t, s, workflow.Active, workflow.Review)
	// Represent an upgraded run awaiting its first reviewer, without a lease.
	if _, err := runtime.DB.ExecContext(ctx, "DELETE FROM agent_port_leases WHERE run_id=?", run.ID); err != nil {
		t.Fatal(err)
	}
	api.issues["owner/repo"] = append(api.issues["owner/repo"], ready(8))
	if _, err := runtime.Workflow.Transition(ctx, "holder", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 8, AgentPorts: &cfg.AgentPorts}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Workflow.Transition(ctx, "holder", workflow.Request{Trigger: workflow.TakeOver}); err != nil {
		t.Fatal(err)
	}
	s, err = scheduler.New(cfg, schedulerResources(runtime), deps)
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
	advanceFixUntil(t, s, runtime, run.ID, func(current workflow.Run) bool {
		return current.State == workflow.WaitingForCI && current.Phase == workflow.Review
	})
}
