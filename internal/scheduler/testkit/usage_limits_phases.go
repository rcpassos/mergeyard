package testkit

import (
	"github.com/rcpassos/mergeyard/internal/harness"
)

func ClassifiedAdapter(cfgAgent string, claude, codex harness.HarnessAdapter) *ClassifiedHarness {
	adapter := claude
	if cfgAgent == "codex" {
		adapter = codex
	}
	return &ClassifiedHarness{HarnessAdapter: adapter, Outcome: harness.FailureClassification{Kind: harness.TemporaryLimit, Source: "phase-fixture"}}
}
