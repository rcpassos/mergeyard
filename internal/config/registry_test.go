package config_test

import (
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/harness/registry"
)

// TestConfigurationAcceptsEveryRegisteredHarness binds the registry to
// configuration validation: a harness the registry lists can be configured.
func TestConfigurationAcceptsEveryRegisteredHarness(t *testing.T) {
	for _, name := range registry.Names() {
		cfg, _, err := config.Parse([]byte("implementer: {agent: " + name + "}\nreviewer: {agent: " + name + "}\n"))
		if err != nil || cfg.Implementer.Agent != name || cfg.Reviewer.Agent != name {
			t.Fatalf("registered harness %q rejected: %+v %v", name, cfg, err)
		}
	}
}
