package scheduler_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestChecksRetryRequiresPortsBeforeGrantingRepair(t *testing.T) {
	for _, handback := range []bool{false, true} {
		name := "implementation"
		if handback {
			name = "handback"
		}
		t.Run(name, func(t *testing.T) {
			_, rt, api, _, cfg, r := localFlow(t, successfulScript)
			cfg.AgentPorts = config.AgentPorts{Base: 65534, Size: 2}
			cfg.Repositories[0].CheckRetries = 0
			cfg.Repositories[0].Checks = []string{"exit 7"}
			if handback {
				cfg.Repositories[0].Checks = []string{"if /usr/bin/grep -q broken feature.txt; then exit 7; fi"}
			}
			deps := scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r}
			s, err := scheduler.New(cfg, schedulerResources(rt), deps)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			var failed workflow.Run
			if handback {
				run := finish(t, s, workflow.Active, workflow.Review)
				if _, err := s.Takeover(ctx, run.ID); err != nil {
					t.Fatal(err)
				}
				if err := r.WriteFile(ctx, filepath.Join(setupWorktree(t, rt, run.ID), "feature.txt"), []byte("broken"), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Handback(ctx, run.ID); err != nil {
					t.Fatal(err)
				}
				failed = finish(t, s, workflow.NeedsAttention, workflow.Review)
			} else {
				failed = finish(t, s, workflow.NeedsAttention, workflow.Implement)
			}
			if failed.LastErrorCode != "checks.failed" {
				t.Fatalf("expected failed checks: %+v", failed)
			}
			// Represent a legacy run with no assignment, while a retained manual
			// run owns the only configured block.
			if _, err := rt.DB.ExecContext(ctx, "DELETE FROM agent_port_leases WHERE run_id=?", failed.ID); err != nil {
				t.Fatal(err)
			}
			api.issues["owner/repo"] = append(api.issues["owner/repo"], ready(8))
			if _, err := rt.Workflow.Transition(ctx, "holder", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 8, AgentPorts: &cfg.AgentPorts}); err != nil {
				t.Fatal(err)
			}
			if _, err := rt.Workflow.Transition(ctx, "holder", workflow.Request{Trigger: workflow.TakeOver}); err != nil {
				t.Fatal(err)
			}
			s, err = scheduler.New(cfg, schedulerResources(rt), deps)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			selected, err := s.Retry(ctx, failed.ID)
			var coded *fault.Error
			if !errors.As(err, &coded) || coded.Code != "agent_ports.exhausted" || selected.State != workflow.NeedsAttention || selected.AgentPorts != nil {
				t.Fatalf("checks Retry bypassed atomic port allocation: state=%s ports=%+v error=%v", selected.State, selected.AgentPorts, err)
			}
			if selected.LastErrorCode != failed.LastErrorCode || selected.LastErrorMessage != failed.LastErrorMessage || selected.PendingRetry() != nil {
				t.Fatalf("exhaustion changed diagnosis or retained pending intent: %+v", selected)
			}
			if len(selected.Retries) != 1 || !strings.HasPrefix(selected.Retries[0].Error, "agent_ports.exhausted:") || len(selected.Handbacks) != len(failed.Handbacks) {
				t.Fatalf("exhaustion granted repair or handback: %+v", selected.Retries)
			}
			issue, err := api.GetIssue(ctx, "owner/repo", failed.IssueNumber)
			if err != nil || !has(issue, "agent-needs-attention") || has(issue, "agent-running") {
				t.Fatalf("exhausted Retry changed progress labels: %+v %v", issue.Labels, err)
			}
			if _, err := s.Reconcile(ctx); err != nil {
				t.Fatalf("rejected Retry blocked reconciliation: %v", err)
			}
			if err := s.Stop(ctx, "holder"); err != nil {
				t.Fatal(err)
			}
			selected, err = s.Retry(ctx, failed.ID)
			wantState := workflow.Active
			if handback {
				wantState = workflow.Manual
			}
			if err != nil || selected.State != wantState || selected.AgentPorts == nil || selected.AgentPorts.Base != 65534 {
				t.Fatalf("checks Retry did not acquire released block: state=%s ports=%+v error=%v", selected.State, selected.AgentPorts, err)
			}
			if len(selected.Retries) != 2 || selected.Retries[1].Error != "" || selected.Retries[1].GrantedCheck != 1 {
				t.Fatalf("successful Retry did not grant exactly one repair: %+v", selected.Retries)
			}
		})
	}
}
