package harness

import (
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/harness/registry"
)

// Adapters builds the adapter for every registered harness. The registry owns
// the harness names; this switch owns how each one is configured.
func Adapters(cfg config.Config) map[string]HarnessAdapter {
	adapters := make(map[string]HarnessAdapter, len(registry.Names()))
	for _, name := range registry.Names() {
		adapters[name] = newAdapter(name, cfg)
	}
	return adapters
}

// newAdapter is total over registry.Names; TestAdaptersCoverEveryRegisteredHarness
// keeps the two in step.
func newAdapter(name string, cfg config.Config) HarnessAdapter {
	switch name {
	case registry.Claude:
		return NewClaude(cfg.Agents.Claude)
	case registry.Codex:
		return NewCodex(cfg.Agents.Codex)
	default:
		panic("harness: " + name + " is registered without an adapter")
	}
}
