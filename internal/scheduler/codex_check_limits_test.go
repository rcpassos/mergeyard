package scheduler_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type codexCheckLimitHarness struct {
	*creditHarness
	checking bool
}

func (h *codexCheckLimitHarness) ClassifyFailure(a harness.PhaseArtifacts, observed time.Time) harness.FailureClassification {
	if h.checking {
		return h.HarnessAdapter.ClassifyFailure(a, observed)
	}
	return h.creditHarness.ClassifyFailure(a, observed)
}

func TestCodexAvailabilityCheckPreservesReportedReset(t *testing.T) {
	previous := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = previous })
	_, rt, api, _, cfg, r := codexFlow(t, `exit 1`)
	h := &codexCheckLimitHarness{creditHarness: &creditHarness{classifiedHarness: &classifiedHarness{HarnessAdapter: harness.NewCodex(cfg.Agents.Codex), outcome: harness.FailureClassification{Kind: harness.CreditsExhausted}}, detection: true}}
	now := time.Date(2026, 10, 16, 12, 23, 36, 0, time.UTC)
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{"codex": h}})
	if err != nil {
		t.Fatal(err)
	}
	finish(t, s, workflow.NeedsAttention, workflow.Implement)
	fixture, err := os.ReadFile("../../docs/research/codex-m3/fixtures/usage-limit-first-request/events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
case "$*" in *"mcp list --json"*) printf '[]\n'; exit 0;; esac
cat <<'EVENTS'
` + string(fixture) + "EVENTS\nexit 1\n"
	if err := os.WriteFile(cfg.Agents.Codex.Executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	h.checking = true
	if _, err := s.CheckHarness(context.Background(), "codex"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		if _, err := s.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		states, err := s.HarnessAvailability(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range states {
			if v.Harness == "codex" && v.Check != nil && v.Check.Status == "released" {
				want := time.Date(2026, 10, 16, 13, 53, 0, 0, time.UTC)
				if v.ResetTimeSource != "reported" || !v.ResetAt.Equal(want) {
					t.Fatalf("availability reset = %+v", v)
				}
				return true
			}
		}
		return false
	})
}
