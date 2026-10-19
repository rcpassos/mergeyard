package scheduler_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/workflow"
)

// A successful implementation can be continued after its phase directory was
// cleaned. Both the continuation and the following review use the report saved
// on the attempt, so no phase file is required.
func TestSuccessfulImplementContinuationWorksWithoutResultFile(t *testing.T) {
	ctx := context.Background()
	s, runtime, api, _, _, _ := localFlow(t, successfulScript)
	run := finish(t, s, workflow.Active, workflow.Review)
	if run.Implementer == nil || run.Implementer.Attempt != 1 {
		t.Fatalf("fixture implementation: %+v", run.Implementer)
	}
	if _, err := runtime.DB.ExecContext(ctx, "UPDATE runs SET state='ACTIVE',current_phase='implement' WHERE id=?", run.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(runtime.Workspace.Root, "runs", run.ID, "phases", "implement-0-1", "result.json")); err != nil {
		t.Fatal(err)
	}

	resumed := finish(t, s, workflow.Active, workflow.Review)
	if resumed.Implementer.Attempt != 1 || len(resumed.ImplementHistory) != 1 || resumed.PRNumber != run.PRNumber || api.creations != 1 {
		t.Fatalf("continuation relaunched or republished work: %+v creations=%d", resumed, api.creations)
	}
	inputPath := filepath.Join(runtime.Workspace.Root, "runs", run.ID, "phases", "review-1-1", "input.md")
	waitForWithin(t, 15*time.Second, func() bool {
		if err := s.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		_, err := os.Stat(inputPath)
		return err == nil
	})
	input, err := os.ReadFile(inputPath)
	if err != nil || !strings.Contains(string(input), "Added feature") {
		t.Fatalf("review did not receive the saved implement report: %s %v", input, err)
	}
}
