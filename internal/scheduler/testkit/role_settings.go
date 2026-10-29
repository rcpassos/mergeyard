package testkit

import (
	"github.com/rcpassos/mergeyard/internal/harness"
)

type HarnessWithoutSkillSelection struct{ harness.HarnessAdapter }

func (h HarnessWithoutSkillSelection) Capabilities() harness.HarnessCapabilities {
	caps := h.HarnessAdapter.Capabilities()
	caps.SkillSelection = false
	return caps
}
