package workflow_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/store"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestAgentPortsSurviveRestartAndReleaseOnlyAtTerminalStates(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	w := workflow.New(db, events.New(db, nil))
	ports := config.AgentPorts{Base: 5100, Size: 10}
	claim := func(id string, issue int) workflow.Run {
		t.Helper()
		run, err := w.Transition(ctx, id, workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: issue, AgentPorts: &ports})
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	first := claim("first", 1)
	second := claim("second", 2)
	if first.AgentPorts == nil || first.AgentPorts.Base != 5100 || second.AgentPorts == nil || second.AgentPorts.Base != 5110 {
		t.Fatalf("overlapping or missing leases: %+v %+v", first.AgentPorts, second.AgentPorts)
	}
	for _, trigger := range []workflow.Trigger{workflow.ClaimSucceeded, workflow.WorktreeReady, workflow.ImplementSucceeded, workflow.ReviewApproved, workflow.CIPassed} {
		if _, err := w.Transition(ctx, first.ID, workflow.Request{Trigger: trigger}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	w = workflow.New(db, events.New(db, nil))
	// Changed configuration cannot relocate live leases or overlap them.
	ports = config.AgentPorts{Base: 5105, Size: 11}
	if err := w.ReconcileAgentPorts(ctx, ports); err != nil {
		t.Fatal(err)
	}
	third := claim("third", 3)
	if third.AgentPorts.Base != 5127 {
		t.Fatalf("new lease overlaps persisted blocks: %+v", third.AgentPorts)
	}
	ready, err := w.Get(ctx, first.ID)
	if err != nil || ready.AgentPorts == nil || *ready.AgentPorts != (config.AgentPorts{Base: 5100, Size: 10}) {
		t.Fatalf("READY_TO_MERGE lost stable lease: %+v %v", ready.AgentPorts, err)
	}
	if _, err := w.Transition(ctx, first.ID, workflow.Request{Trigger: workflow.TakeOver}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Transition(ctx, first.ID, workflow.Request{Trigger: workflow.HandBack, NextPhase: workflow.Review}); err != nil {
		t.Fatal(err)
	}
	for _, trigger := range []workflow.Trigger{workflow.ReviewApproved, workflow.CIPassed} {
		if _, err := w.Transition(ctx, first.ID, workflow.Request{Trigger: trigger}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		id      string
		trigger workflow.Trigger
	}{
		{first.ID, workflow.PRMerged}, {second.ID, workflow.Stop}, {third.ID, workflow.InternalFailure},
	} {
		run, err := w.Transition(ctx, tc.id, workflow.Request{Trigger: tc.trigger, Failure: &fault.Error{Code: "test.failed", Message: "Test failure"}})
		if err != nil || run.AgentPorts != nil {
			t.Fatalf("terminal lease: %+v %v", run.AgentPorts, err)
		}
	}
	ports = config.AgentPorts{Base: 5100, Size: 10}
	if run := claim("reused", 4); run.AgentPorts.Base != 5100 {
		t.Fatalf("released range not reused: %+v", run.AgentPorts)
	}
}

func TestAgentPortsReconcileLegacyRunsAndExhaustionIsAtomic(t *testing.T) {
	w, _, _ := newWorkflow(t)
	ctx := context.Background()
	legacy := seed(t, w, workflow.NeedsAttention, workflow.Implement)
	ports := config.AgentPorts{Base: 65534, Size: 2}
	if err := w.ReconcileAgentPorts(ctx, ports); err != nil {
		t.Fatal(err)
	}
	got, err := w.Get(ctx, legacy.ID)
	if err != nil || got.AgentPorts == nil || got.AgentPorts.Base != 65534 {
		t.Fatalf("legacy lease: %+v %v", got.AgentPorts, err)
	}
	if _, err := w.Transition(ctx, "exhausted", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 9, AgentPorts: &ports}); err == nil {
		t.Fatal("claimed without an available port block")
	}
	if _, err := w.Get(ctx, "exhausted"); err == nil {
		t.Fatal("exhausted claim was partially committed")
	}
}

func TestConcurrentClaimsReceiveDistinctAgentPortBlocks(t *testing.T) {
	w, _, _ := newWorkflow(t)
	ports := config.AgentPorts{Base: 5100, Size: 10}
	const count = 8
	runs := make(chan workflow.Run, count)
	errors := make(chan error, count)
	var group sync.WaitGroup
	for i := range count {
		group.Add(1)
		go func() {
			defer group.Done()
			run, err := w.Transition(context.Background(), fmt.Sprintf("run-%d", i), workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: i + 1, AgentPorts: &ports})
			if err != nil {
				errors <- err
				return
			}
			runs <- run
		}()
	}
	group.Wait()
	close(runs)
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for run := range runs {
		if run.AgentPorts == nil || seen[run.AgentPorts.Base] {
			t.Fatalf("missing or overlapping block: %+v", run.AgentPorts)
		}
		seen[run.AgentPorts.Base] = true
	}
	for _, base := range []int{5100, 5110, 5120, 5130, 5140, 5150, 5160, 5170} {
		if !seen[base] {
			t.Fatalf("expected occupied block at %d", base)
		}
	}
}

func TestFailedRunRetryAllocatesAroundReusedAgentPorts(t *testing.T) {
	w, _, _ := newWorkflow(t)
	ctx := context.Background()
	ports := config.AgentPorts{Base: 5100, Size: 10}
	run := seed(t, w, workflow.Active, workflow.Implement)
	if _, err := w.EnsureAgentPorts(ctx, run.ID, ports); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.InternalFailure, Failure: &fault.Error{Code: "test.failed", Message: "Test failure"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.EnsureAgentPorts(ctx, run.ID, ports); err == nil {
		t.Fatal("terminal run reserved ports")
	}
	if _, err := w.Transition(ctx, "new-run", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 6, AgentPorts: &ports}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.Retry, NextState: workflow.Active, NextPhase: workflow.Implement, AgentPorts: &ports}); err != nil {
		t.Fatal(err)
	}
	got, err := w.EnsureAgentPorts(ctx, run.ID, ports)
	if err != nil || got.Base != 5110 {
		t.Fatalf("retry collided with reused ports: %+v %v", got, err)
	}
}

func TestAgentPortExhaustionDoesNotPartiallyCommitRetry(t *testing.T) {
	w, _, _ := newWorkflow(t)
	ctx := context.Background()
	ports := config.AgentPorts{Base: 65534, Size: 2}
	failed := seed(t, w, workflow.Active, workflow.Implement)
	if _, err := w.Transition(ctx, failed.ID, workflow.Request{Trigger: workflow.InternalFailure, Failure: &fault.Error{Code: "test.failed", Message: "Test failure"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Transition(ctx, "holder", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 6, AgentPorts: &ports}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Transition(ctx, failed.ID, workflow.Request{Trigger: workflow.Retry, NextState: workflow.Active, NextPhase: workflow.Implement, AgentPorts: &ports}); err == nil {
		t.Fatal("Retry succeeded without available ports")
	}
	run, err := w.Get(ctx, failed.ID)
	if err != nil || run.State != workflow.Failed || run.AgentPorts != nil {
		t.Fatalf("partially committed Retry: %+v %v", run, err)
	}
}
