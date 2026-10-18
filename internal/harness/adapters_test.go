package harness_test

import (
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/harness/registry"
)

func TestAdaptersCoverEveryRegisteredHarness(t *testing.T) {
	adapters := harness.Adapters(config.Config{})
	if len(adapters) != len(registry.Names()) {
		t.Fatalf("adapters = %d for %d registered harnesses", len(adapters), len(registry.Names()))
	}
	for _, name := range registry.Names() {
		adapter := adapters[name]
		if adapter == nil || adapter.Type() != name {
			t.Fatalf("registered harness %q has no adapter", name)
		}
	}
}
