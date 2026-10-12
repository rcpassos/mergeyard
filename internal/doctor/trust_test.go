package doctor_test

import (
	"context"
	"testing"

	"github.com/rcpassos/mergeyard/internal/doctor"
	"github.com/rcpassos/mergeyard/internal/runner"
)

func TestPublicRepositoryWithoutExplicitTrustWarns(t *testing.T) {
	for _, tc := range []struct {
		name, settings string
		private, warn  bool
	}{
		{"public defaults", "", false, true},
		{"public explicit", "trusted_labelers: [alice]\n", false, false},
		{"private defaults", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, fake, opts := repositorySetup(t, tc.settings)
			body := `{"private":false,"permissions":{"push":true}}`
			if tc.private {
				body = `{"private":true,"permissions":{"push":true}}`
			}
			fake.responses["gh api --hostname github.com --method GET repos/octo/repo"] = runner.ExecResult{Stdout: []byte(body)}
			report := doctor.Check(context.Background(), path, opts)
			found := false
			for _, finding := range report.Findings {
				if finding.Code == "github.public_without_trusted_labelers" && finding.Severity == doctor.Warning {
					found = true
				}
			}
			if found != tc.warn {
				t.Fatalf("warning=%v, want %v: %+v", found, tc.warn, report)
			}
		})
	}
}
