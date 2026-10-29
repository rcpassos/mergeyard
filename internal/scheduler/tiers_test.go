package scheduler_test

import (
	"context"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestInvalidTierSelectionRefusesClaimOnceAndAllowsCorrection(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		Labels []string
	}{
		{"unknown", []string{"mergeyard:implementer-tier=unknown"}},
		{"conflicting", []string{"mergeyard:implementer-tier=low", "mergeyard:implementer-tier=high"}},
		{"undefined", []string{"mergeyard:reviewer-tier=medium"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			labels := tc.Labels
			flow := testkit.NewScenario(t)
			_, _, api, _, cfg, r := flow.Parts()
			for _, label := range labels {
				api.Issues["owner/repo"][0].Labels = append(api.Issues["owner/repo"][0].Labels, github.Label{Name: label})
			}
			deps := scheduler.Dependencies{GitHub: api, Runner: r}
			s, err := flow.New(cfg, deps)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := s.Plan(context.Background(), "")
			if err != nil || len(plan.Issues) != 1 || plan.Issues[0].Decision != "skipped" || len(api.OverrideComments) != 0 {
				t.Fatalf("invalid plan selection: %+v %v", plan, err)
			}
			for range 2 {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			s, err = flow.New(cfg, deps)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			runs, err := s.Runs(context.Background())
			if err != nil || len(runs) != 0 || len(api.OverrideComments) != 1 || !testkit.Has(api.Issues["owner/repo"][0], cfg.Repositories[0].Labels.NeedsAttention) {
				t.Fatalf("invalid selection claimed or replayed: %+v %v comments=%+v", runs, err, api.OverrideComments)
			}
			for _, label := range labels {
				if !strings.Contains(api.OverrideComments[7], label) {
					t.Fatalf("diagnostic missing %q: %s", label, api.OverrideComments[7])
				}
			}
			api.Issues["owner/repo"][0].Labels = []github.Label{{Name: cfg.Repositories[0].Labels.Ready}}
			claimed := flow.Until(s, workflow.Active, workflow.Review)
			if claimed.RoleSettings.Implementer.Tier != "" || claimed.RoleSettings.Reviewer.Tier != "" || len(api.OverrideComments) != 1 {
				t.Fatalf("corrected selection: %+v", claimed.RoleSettings)
			}
		})
	}
}
