package config_test

import (
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
)

func TestAgentPortsConfiguration(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		want config.AgentPorts
	}{
		{"repositories: []", config.AgentPorts{Base: 5100, Size: 10}},
		{"agent_ports: {base: 6100, size: 11}", config.AgentPorts{Base: 6100, Size: 11}},
	} {
		cfg, doc, err := config.Parse([]byte(tc.yaml))
		if err != nil || cfg.AgentPorts != tc.want || len(doc.Warnings) != 0 {
			t.Fatalf("%s: config=%+v warnings=%+v error=%v", tc.yaml, cfg.AgentPorts, doc, err)
		}
	}
	for _, yaml := range []string{
		"agent_ports: {base: 0}", "agent_ports: {base: 65536}",
		"agent_ports: {size: 1}", "agent_ports: {size: -2}",
		"agent_ports: {size: 2.5}", "agent_ports: {base: 65530, size: 10}",
		"agent_ports: {base: 65520, size: 10}\nconcurrency: 2",
	} {
		if _, _, err := config.Parse([]byte(yaml)); err == nil {
			t.Fatalf("accepted invalid port configuration: %s", yaml)
		}
	}
}
