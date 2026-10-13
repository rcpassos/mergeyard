package config_test

import (
	"github.com/rcpassos/mergeyard/internal/config"
	"testing"
)

func TestVerificationConfigDistinguishesOmittedAndDisabled(t *testing.T) {
	for _, tc := range []struct {
		name, yaml       string
		nilList, invalid bool
		count            int
	}{
		{name: "omitted", nilList: true},
		{name: "disabled", yaml: "    verify_commands: []\n"},
		{name: "configured", yaml: "    verify_commands: [go test ./...]\n", count: 1},
		{name: "null is not disabled", yaml: "    verify_commands: null\n", invalid: true},
		{name: "blank command", yaml: "    verify_commands: [' ']\n", invalid: true},
		{name: "NUL command", yaml: "    verify_commands: [\"a\\0b\"]\n", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, doc, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n    checks: [npm test]\n" + tc.yaml))
			if tc.invalid {
				if err == nil {
					t.Fatal("accepted invalid verification")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			commands := cfg.Repositories[0].VerifyCommands
			if (commands == nil) != tc.nilList || len(commands) != tc.count || len(doc.Warnings) != 0 {
				t.Fatalf("commands=%#v warnings=%v", commands, doc.Warnings)
			}
		})
	}
}
