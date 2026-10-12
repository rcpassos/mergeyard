package config_test

import (
	"github.com/rcpassos/mergeyard/internal/config"
	"testing"
)

func TestRepositoryChecksDefaultsAndValidation(t *testing.T) {
	for _, tc := range []struct {
		yaml    string
		retries int
		count   int
		invalid bool
	}{
		{"", 1, 0, false},
		{"    checks: [\"go test ./...\", \"go vet ./...\"]\n", 1, 2, false},
		{"    checks: []\n    check_retries: 0\n", 0, 0, false},
		{"    check_retries: -1\n", 0, 0, true},
		{"    checks: [\" \" ]\n", 0, 0, true},
	} {
		cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n" + tc.yaml))
		if tc.invalid {
			if err == nil {
				t.Fatalf("accepted invalid checks: %s", tc.yaml)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Repositories[0].CheckRetries != tc.retries || len(cfg.Repositories[0].Checks) != tc.count {
			t.Fatalf("checks config=%+v", cfg.Repositories[0])
		}
	}
}
