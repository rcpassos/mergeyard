package testkit

import (
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/harness"
)

// These fixtures return normalized restrictions through the approved harness
// seam; they do not claim native exhausted-credit signal support.
func ManualLimitHarness(cfg config.Config, agent string, kind harness.FailureKind) *CreditHarness {
	adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
	if agent == "codex" {
		adapter = harness.NewCodex(cfg.Agents.Codex)
	}
	return &CreditHarness{ClassifiedHarness: &ClassifiedHarness{HarnessAdapter: adapter, Outcome: harness.FailureClassification{Kind: kind, Source: "classified-fixture"}}, Detection: true}
}
