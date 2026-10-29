package testkit

import (
	"fmt"

	"github.com/rcpassos/mergeyard/internal/harness"
)

type ProofCrashHarness struct{ harness.HarnessAdapter }

func (h ProofCrashHarness) NativeSucceeded(a harness.PhaseArtifacts) bool {
	if h.HarnessAdapter.NativeSucceeded(a) {
		fmt.Println("review-control-plane-ready")
		select {}
	}
	return false
}
