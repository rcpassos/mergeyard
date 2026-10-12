package doctor_test

import (
	"context"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/doctor"
)

func TestDoctorWarnsWhenAgentPortBlocksOverlapDashboard(t *testing.T) {
	for _, tc := range []struct {
		offset, concurrency int
		overlap             bool
	}{
		{0, 1, true}, {-11, 2, true}, {-11, 1, true}, {1, 1, false},
	} {
		path, _, opts := repositorySetup(t, "")
		cfg, _, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		setConfig(t, path, "agent_ports", map[string]int{"base": cfg.Port + tc.offset, "size": 10})
		setConfig(t, path, "concurrency", tc.concurrency)
		report := doctor.Check(context.Background(), path, opts)
		found := false
		for _, finding := range report.Findings {
			if finding.Code == "agent_ports.dashboard_overlap" {
				found = finding.Severity == doctor.Warning
			}
		}
		if found != tc.overlap {
			t.Fatalf("overlap=%t want %t: %+v", found, tc.overlap, report)
		}
	}
}
