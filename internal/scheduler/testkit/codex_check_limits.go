package testkit

import (
	"time"

	"github.com/rcpassos/mergeyard/internal/harness"
)

type CodexCheckLimitHarness struct {
	*CreditHarness
	Checking bool
}

func (h *CodexCheckLimitHarness) ClassifyFailure(a harness.PhaseArtifacts, observed time.Time) harness.FailureClassification {
	if h.Checking {
		return h.HarnessAdapter.ClassifyFailure(a, observed)
	}
	return h.CreditHarness.ClassifyFailure(a, observed)
}
