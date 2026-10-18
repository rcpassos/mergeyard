package doctor

import (
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/harness/registry"
)

// TestEveryRegisteredHarnessHasDoctorFacts keeps the harness registry and the
// doctor's per-harness probes in step: a registered harness without facts would
// otherwise be probed with another harness's executable, version minimum and
// login check.
func TestEveryRegisteredHarnessHasDoctorFacts(t *testing.T) {
	configured := config.Config{Agents: config.Agents{
		Claude: config.Claude{Executable: "claude"},
		Codex:  config.Codex{Executable: "codex"},
	}}
	for _, name := range registry.Names() {
		facts, ok := harnesses[name]
		if !ok {
			t.Fatalf("registered harness %q has no doctor facts", name)
		}
		if facts.executable == nil || facts.executable(configured) == "" {
			t.Fatalf("registered harness %q has no executable resolver", name)
		}
		if facts.minimum == (version{}) || len(facts.auth) == 0 || facts.effortFlag == "" || facts.skillDir == "" {
			t.Fatalf("registered harness %q has incomplete doctor facts: %+v", name, facts)
		}
	}
}
