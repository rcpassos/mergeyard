package scheduler_test

import (
	"context"
	"testing"

	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// TestStopWithoutAnIntentChangesNothingInAnyState proves that a run whose
// preparation never created a branch or worktree has nothing to release: it stops without a flag or an
// explanation.
func TestStopWithoutAnIntentChangesNothingInAnyState(t *testing.T) {
	t.Parallel()

	for _, state := range testkit.UnownedStates {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()

			flow := testkit.NewScenario(t)
			s, rt, api, _, _, _ := flow.Parts()
			ctx := context.Background()
			run := testkit.RunInState(t, rt, "untouched", state)
			if err := s.Stop(ctx, run.ID); err != nil {
				t.Fatal(err)
			}
			stopped, err := rt.Workflow.Get(ctx, run.ID)
			if err != nil || stopped.State != workflow.Stopped {
				t.Fatalf("stop: %+v %v", stopped, err)
			}
			if testkit.Has(api.Issues["owner/repo"][0], "agent-needs-attention") {
				t.Fatalf("a run without leftovers flagged the issue: %+v", api.Issues["owner/repo"][0].Labels)
			}
			if _, ok := api.LeftoverComments[7]; ok {
				t.Fatalf("a run without leftovers was explained: %+v", api.LeftoverComments)
			}
		})
	}
}
