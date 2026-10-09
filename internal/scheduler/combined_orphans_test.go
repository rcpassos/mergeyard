package scheduler_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
)

func TestOrphansRemainVisibleAndPreservedBehindDispatchGates(t *testing.T) {
	for _, gate := range []string{"paused", "disabled", "capacity"} {
		t.Run(gate, func(t *testing.T) {
			ctx := context.Background()
			s, rt, api, _, cfg, r := localFlow(t, `/bin/sleep 60`)
			if gate == "capacity" {
				if err := s.Tick(ctx); err != nil {
					t.Fatal(err)
				}
			} else if gate == "disabled" {
				cfg.Repositories[0].Enabled = false
				var err error
				s, err = scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
				if err != nil {
					t.Fatal(err)
				}
			} else if err := s.Pause(ctx, true); err != nil {
				t.Fatal(err)
			}
			issue := ready(42)
			issue.Labels = append(issue.Labels, github.Label{Name: "agent-running"})
			api.issues["owner/repo"] = append(api.issues["owner/repo"], issue)
			api.mutate = func(_ string, _ string, n int, _ string) error {
				if n == 42 {
					t.Fatal("ambiguous claim labels changed")
				}
				return nil
			}
			tree, err := managedgit.New(rt.Workspace).Prepare(ctx, managedgit.PrepareRequest{Repository: "owner/repo", RunID: "orphan", IssueNumber: 42})
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(tree.Path, "keep.txt")
			if err := os.WriteFile(marker, []byte("preserve me"), 0600); err != nil {
				t.Fatal(err)
			}
			ref, err := r.StartSession(ctx, runner.SessionRequest{RunID: "orphan", Phase: "implement", Attempt: 1, PhaseDir: filepath.Join(rt.Workspace.Root, "runs", "orphan", "phases", "implement-0-1"), Command: runner.ExecRequest{Executable: "/bin/sleep", Args: []string{"60"}, Dir: tree.Path}})
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := s.Tick(ctx); err != nil {
					t.Fatal(err)
				}
				report, err := s.Reconcile(ctx)
				if err != nil || len(report.Findings) != 3 {
					t.Fatalf("orphans hidden behind %s: %+v %v", gate, report, err)
				}
			}
			history, err := rt.Events.History(ctx, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			counts := map[string]int{}
			for _, event := range history {
				var finding scheduler.Finding
				if err := json.Unmarshal(event.Payload, &finding); err != nil {
					t.Fatal(err)
				}
				if finding.IssueNumber == 42 || finding.Session == ref.Name || finding.Code == "reconcile.orphaned_worktree" {
					counts[event.Type]++
				}
			}
			for _, code := range []string{"reconcile.orphaned_claim", "reconcile.orphaned_worktree", "reconcile.orphaned_session"} {
				if counts[code] != 1 {
					t.Fatalf("unchanged finding emitted %d times: %s", counts[code], code)
				}
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "preserve me" {
				t.Fatalf("orphan work changed: %s %v", data, err)
			}
			if status, err := r.SessionStatus(ctx, ref); err != nil || status.State != runner.SessionRunning {
				t.Fatalf("orphan process reclaimed: %+v %v", status, err)
			}
			runs, err := s.Runs(ctx)
			want := 0
			if gate == "capacity" {
				want = 1
			}
			if err != nil || len(runs) != want || !has(api.issues["owner/repo"][1], "agent-running") {
				t.Fatalf("orphan adopted or claim cleared: %+v %v", runs, err)
			}
		})
	}
}
