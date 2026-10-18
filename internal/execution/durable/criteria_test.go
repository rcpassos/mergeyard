package durable_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// The saved version table decides whether criteria.json is required; the phase
// alone never does.
func TestSavedContractRequiresCriteriaFile(t *testing.T) {
	e, _, run, _ := executor(t)
	for _, tc := range []struct {
		name, report string
		phase        workflow.Phase
		version      int
		criteria     string
		wantError    bool
	}{
		{
			name: "review v2 without criteria", phase: workflow.Review, version: 2,
			report: `{"schema_version":2,"status":"approved","summary":"Clean","findings":[],"prior_findings":[]}`,
		},
		{
			name: "review v4 without criteria", phase: workflow.Review, version: 4,
			report:    `{"schema_version":4,"criteria":[],"human_action":null,"status":"approved","summary":"Clean","findings":[],"prior_findings":[]}`,
			wantError: true,
		},
		{
			name: "review v4 with matching criteria", phase: workflow.Review, version: 4,
			criteria: `[{"id":"AC1","text":"Runs"}]`,
			report:   `{"schema_version":4,"criteria":[{"id":"AC1","verdict":"unverified","note":"Not checked"}],"human_action":null,"status":"approved","summary":"Clean","findings":[],"prior_findings":[]}`,
		},
		{
			name: "review v4 with missing criterion", phase: workflow.Review, version: 4,
			criteria:  `[{"id":"AC1","text":"Runs"}]`,
			report:    `{"schema_version":4,"criteria":[],"human_action":null,"status":"approved","summary":"Clean","findings":[],"prior_findings":[]}`,
			wantError: true,
		},
		{
			name: "implement v2 without criteria", phase: workflow.Implement, version: 2,
			report: `{"schema_version":2,"human_action":null,"status":"success","summary":"Done"}`,
		},
		{
			name: "implement v3 without criteria", phase: workflow.Implement, version: 3,
			report:    `{"schema_version":3,"criteria":[],"human_action":null,"status":"success","summary":"Done"}`,
			wantError: true,
		},
		{
			name: "fix v3 without criteria", phase: workflow.Fix, version: 3,
			report:    `{"schema_version":3,"criteria":[],"human_action":null,"status":"success","summary":"Done","responses":[]}`,
			wantError: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			phaseDir := t.TempDir()
			schema := `{"properties":{"schema_version":{"const":` + strconv.Itoa(tc.version) + `}}}`
			if err := os.WriteFile(filepath.Join(phaseDir, "schema.json"), []byte(schema), 0600); err != nil {
				t.Fatal(err)
			}
			if tc.criteria != "" {
				if err := os.WriteFile(filepath.Join(phaseDir, "criteria.json"), []byte(tc.criteria), 0600); err != nil {
					t.Fatal(err)
				}
			}
			artifacts := harness.PhaseArtifacts{Stdout: []byte(`{"type":"result","is_error":false,"structured_output":` + tc.report + `}`)}
			_, err := e.Parse(context.Background(), run.ID, "", "claude", harness.PhaseContext{Phase: tc.phase, PhaseDir: phaseDir}, artifacts)
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v, wantError %v", err, tc.wantError)
			}
		})
	}
}
