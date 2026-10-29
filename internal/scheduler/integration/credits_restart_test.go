package integration_test

import (
	"context"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestCreditProbeSurvivesControlPlaneKill(t *testing.T) {
	t.Parallel()

	for _, agent := range []string{"claude", "codex"} {
		for _, boundary := range []string{"probe-reserved", "resuming", "before-proof", "after-proof"} {
			t.Run(agent+"/"+boundary, func(t *testing.T) {
				t.Parallel()

				_, initial, api, _, cfg, r := testkit.TmuxPairingFlow(t, agent, agent, testkit.DisputedReport)
				testkit.ReplacePhaseScript(t, cfg, workflow.Implement, `case "$*" in *implement-0-1*) printf partial > partial.txt; exit 1;; esac
`+testkit.SuccessfulScript)
				adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
				if agent == "codex" {
					adapter = harness.NewCodex(cfg.Agents.Codex)
				}
				h := &testkit.CreditHarness{ClassifiedHarness: &testkit.ClassifiedHarness{HarnessAdapter: adapter, Outcome: harness.FailureClassification{Kind: harness.CreditsExhausted}}, Detection: true}
				s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(initial), scheduler.Dependencies{GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{agent: h}})
				if err != nil {
					t.Fatal(err)
				}
				attention := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
				root := initial.Workspace.Root
				if err := initial.Close(); err != nil {
					t.Fatal(err)
				}
				socket := fmt.Sprintf("mergeyard-credit-crash-%d", time.Now().UnixNano())
				t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
				testkit.KillUsageControlPlane(t, testkit.UsageProcessInput{Config: cfg, Workspace: root, Socket: socket, Agent: agent, Boundary: boundary, Credits: true, Now: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)})
				rt, err := app.Open(context.Background(), root)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { rt.Close() })
				restarted, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: testkit.NewLocalRunner(t, runner.Options{SocketName: socket}), Harnesses: map[string]harness.HarnessAdapter{agent: h}})
				if err != nil {
					t.Fatal(err)
				}
				// Recovery may already have launched the independent reviewer. Stop
				// its owned process group before closing SQLite, killing tmux, or
				// removing temporary worktrees; killing the server alone can race
				// with a reviewer that is still writing files.
				t.Cleanup(func() {
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					if err := restarted.Stop(ctx, attention.ID); err != nil {
						t.Errorf("stop recovered run before cleanup: %v", err)
					}
				})
				saved, err := rt.Workflow.Get(context.Background(), attention.ID)
				if err != nil || len(saved.CreditProbes) != 1 {
					t.Fatalf("durable owner: %+v %v", saved, err)
				}
				if saved.PRNumber > 0 {
					pr := &github.PullRequest{Number: saved.PRNumber, State: github.Open, Draft: true}
					pr.Head.Ref = "mergeyard/issue-7"
					pr.Head.SHA = api.Head(pr.Head.Ref)
					pr.Head.Repo.FullName = "owner/repo"
					pr.Base.Ref = "main"
					api.Prs = map[string]*github.PullRequest{pr.Head.Ref: pr}
				}
				done := testkit.Finish(t, restarted, workflow.Active, workflow.Review)
				if done.Implementer.Attempt != 2 || done.Implementer.SessionID != attention.Implementer.SessionID || len(done.CreditProbes) != 1 || done.CreditProbes[0].Status != "recovered" || api.Creations > 1 {
					t.Fatalf("duplicated/lost probe: %+v", done)
				}
				states, err := restarted.HarnessAvailability(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				for _, v := range states {
					if v.Harness == agent && !v.Available {
						t.Fatalf("lost completion: %+v", v)
					}
				}
			})
		}
	}
}
