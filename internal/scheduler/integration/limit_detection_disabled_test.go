package integration_test

import (
	"context"
	"testing"

	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestProductionHarnessLimitFailuresConsumeAttemptBudget(t *testing.T) {
	t.Parallel()

	for _, agent := range []string{"claude", "codex"} {
		for _, phase := range []workflow.Phase{workflow.Implement, workflow.Review, workflow.Fix} {
			t.Run(agent+"/"+string(phase), func(t *testing.T) {
				t.Parallel()

				_, rt, api, _, cfg, r := testkit.TmuxPairingFlow(t, agent, agent, testkit.DisputedReport)
				cfg.Repositories[0].Implementer.MaxAttempts = 2
				cfg.Repositories[0].Reviewer.MaxAttempts = 2
				// Diagnostic text is deliberately unverified. Neither production
				// adapter may interpret it as a native limit signal.
				script := `case "$*" in *` + string(phase) + `-*) printf 'usage limit; out of credits; spend cap\n'; exit 1;; esac
` + testkit.LoopScript(testkit.DisputedReport)
				testkit.ReplacePhaseScript(t, cfg, phase, script)
				adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
				if agent == "codex" {
					adapter = harness.NewCodex(cfg.Agents.Codex)
				}
				if c := adapter.Capabilities(); !c.TemporaryLimitDetection || c.CreditExhaustionDetection {
					t.Fatalf("unexpected production detection capabilities: %+v", c)
				}
				s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
				if err != nil {
					t.Fatal(err)
				}
				run := testkit.Finish(t, s, workflow.NeedsAttention, phase)
				attempts := run.Implementer.Attempt
				if phase == workflow.Review {
					attempts = run.Review.Attempt
				} else if phase == workflow.Fix {
					attempts = run.Fix.Attempt
				}
				if attempts != 2 || run.HarnessWait != nil || len(run.HarnessWaitHistory) != 0 || len(run.CreditProbes) != 0 || run.LastErrorCode == "harness.credits_exhausted" || run.LastErrorCode == "harness.usage_limit_waits_exhausted" {
					t.Fatalf("ordinary failure bypassed attempt budget: %+v", run)
				}
				states, err := s.HarnessAvailability(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				for _, state := range states {
					if !state.Available || state.CheckEligible || state.ProbeID != "" {
						t.Fatalf("ordinary failure changed account availability: %+v", state)
					}
				}
				if _, err := s.CheckHarness(context.Background(), agent); err == nil {
					t.Fatal("credit-detection-disabled harness allowed a paid availability request")
				}
			})
		}
	}
}
