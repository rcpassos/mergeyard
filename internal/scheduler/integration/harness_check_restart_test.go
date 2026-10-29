package integration_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestHarnessCheckSurvivesControlPlaneKill(t *testing.T) {
	t.Parallel()

	for _, agent := range []string{"claude", "codex"} {
		for _, boundary := range []string{"resuming", "before-proof", "after-proof"} {
			t.Run(agent+"/"+boundary, func(t *testing.T) {
				t.Parallel()

				_, initial, api, _, cfg, r := testkit.TmuxPairingFlow(t, agent, agent, testkit.DisputedReport)
				testkit.ReplacePhaseScript(t, cfg, workflow.Implement, `exit 1`)
				adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
				if agent == "codex" {
					adapter = harness.NewCodex(cfg.Agents.Codex)
				}
				h := &testkit.CreditHarness{ClassifiedHarness: &testkit.ClassifiedHarness{HarnessAdapter: adapter, Outcome: harness.FailureClassification{Kind: harness.CreditsExhausted}}, Detection: true}
				s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(initial), scheduler.Dependencies{GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{agent: h}})
				if err != nil {
					t.Fatal(err)
				}
				run := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
				if err := s.Stop(context.Background(), run.ID); err != nil {
					t.Fatal(err)
				}
				calls := filepath.Join(t.TempDir(), "launch-count")
				script := `printf 'request\n' >> '` + calls + `'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"OK"}'`
				executable := cfg.Agents.Claude.Executable
				if agent == "codex" {
					executable = cfg.Agents.Codex.Executable
					script = `printf 'request\n' >> '` + calls + `'
printf '%s\n' '{"type":"turn.completed"}'`
				}
				if agent == "codex" {
					script = `case "$*" in *"mcp list --json"*) printf '[]\n'; exit 0;; esac` + "\n" + script
				}
				if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+script), 0700); err != nil {
					t.Fatal(err)
				}
				root := initial.Workspace.Root
				if err := initial.Close(); err != nil {
					t.Fatal(err)
				}
				socket := fmt.Sprintf("mergeyard-check-crash-%d", time.Now().UnixNano())
				t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
				testkit.KillUsageControlPlane(t, testkit.UsageProcessInput{Config: cfg, Workspace: root, Socket: socket, Agent: agent, Boundary: boundary, Credits: true, Check: true, Now: time.Now().UTC()})
				rt, err := app.Open(context.Background(), root)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { rt.Close() })
				s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: testkit.NewLocalRunner(t, runner.Options{SocketName: socket}), Harnesses: map[string]harness.HarnessAdapter{agent: h}})
				if err != nil {
					t.Fatal(err)
				}
				testkit.WaitFor(t, func() bool {
					if _, err := s.Reconcile(context.Background()); err != nil {
						t.Fatal(err)
					}
					states, _ := s.HarnessAvailability(context.Background())
					for _, v := range states {
						if v.Harness == agent {
							return v.Available && v.Check != nil && v.Check.Status == "recovered"
						}
					}
					return false
				})
				saved, err := rt.Workflow.Get(context.Background(), run.ID)
				if err != nil || saved.State != workflow.Stopped {
					t.Fatalf("stopped run changed: %+v %v", saved, err)
				}
				data, err := os.ReadFile(calls)
				if err != nil || strings.Count(string(data), "request") != 1 {
					t.Fatalf("duplicated request: %s %v", data, err)
				}
				history, err := rt.Events.History(context.Background(), 0, 100)
				if err != nil {
					t.Fatal(err)
				}
				proofs := 0
				for _, event := range history {
					if event.Type == "harness.available" {
						proofs++
					}
				}
				if proofs != 1 {
					t.Fatalf("proof recorded %d times", proofs)
				}
			})
		}
	}
}
