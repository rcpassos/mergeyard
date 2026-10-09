package scheduler_test

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
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type proofCrashHarness struct{ harness.HarnessAdapter }

func (h proofCrashHarness) NativeSucceeded(a harness.PhaseArtifacts) bool {
	if h.HarnessAdapter.NativeSucceeded(a) {
		fmt.Println("review-control-plane-ready")
		select {}
	}
	return false
}

func TestCreditProbeSurvivesControlPlaneKill(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		for _, boundary := range []string{"probe-reserved", "resuming", "before-proof", "after-proof"} {
			t.Run(agent+"/"+boundary, func(t *testing.T) {
				_, initial, api, _, cfg, r := pairingFlow(t, agent, agent, disputedReport)
				replacePhaseScript(t, cfg, workflow.Implement, `case "$*" in *implement-0-1*) printf partial > partial.txt; exit 1;; esac
`+successfulScript)
				adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
				if agent == "codex" {
					adapter = harness.NewCodex(cfg.Agents.Codex)
				}
				h := &creditHarness{classifiedHarness: &classifiedHarness{HarnessAdapter: adapter, outcome: harness.FailureClassification{Kind: harness.CreditsExhausted}}, detection: true}
				s, err := scheduler.New(cfg, schedulerResources(initial), scheduler.Dependencies{GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{agent: h}})
				if err != nil {
					t.Fatal(err)
				}
				attention := finish(t, s, workflow.NeedsAttention, workflow.Implement)
				root := initial.Workspace.Root
				if err := initial.Close(); err != nil {
					t.Fatal(err)
				}
				socket := fmt.Sprintf("mergeyard-credit-crash-%d", time.Now().UnixNano())
				t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
				killUsageControlPlane(t, usageProcessInput{Config: cfg, Workspace: root, Socket: socket, Agent: agent, Boundary: boundary, Credits: true, Now: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)})
				rt, err := app.Open(context.Background(), root)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { rt.Close() })
				restarted, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: runner.NewLocal(runner.Options{SocketName: socket}), Harnesses: map[string]harness.HarnessAdapter{agent: h}})
				if err != nil {
					t.Fatal(err)
				}
				saved, err := rt.Workflow.Get(context.Background(), attention.ID)
				if err != nil || len(saved.CreditProbes) != 1 {
					t.Fatalf("durable owner: %+v %v", saved, err)
				}
				if saved.PRNumber > 0 {
					pr := &github.PullRequest{Number: saved.PRNumber, State: github.Open, Draft: true}
					pr.Head.Ref = "mergeyard/issue-7"
					pr.Head.SHA = api.head(pr.Head.Ref)
					pr.Head.Repo.FullName = "owner/repo"
					pr.Base.Ref = "main"
					api.prs = map[string]*github.PullRequest{pr.Head.Ref: pr}
				}
				done := finish(t, restarted, workflow.Active, workflow.Review)
				if done.Implementer.Attempt != 2 || done.Implementer.SessionID != attention.Implementer.SessionID || len(done.CreditProbes) != 1 || done.CreditProbes[0].Status != "recovered" || api.creations > 1 {
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
