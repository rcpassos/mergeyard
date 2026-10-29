package scheduler_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestHarnessOverrideRequiresDestinationCapabilities(t *testing.T) {
	t.Parallel()

	flow := testkit.NewScenario(t)
	_, _, api, _, cfg, r := flow.Parts()
	cfg.Repositories[0].Implementer.Skills = []string{"check"}
	api.Issues["owner/repo"][0].Labels = append(api.Issues["owner/repo"][0].Labels, github.Label{Name: "mergeyard:implementer=codex"})
	s, err := flow.New(cfg, scheduler.Dependencies{GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{"codex": testkit.HarnessWithoutSkillSelection{HarnessAdapter: harness.NewCodex(cfg.Agents.Codex)}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(context.Background())
	if err != nil || len(runs) != 0 || !strings.Contains(api.OverrideComments[7], "harness.capability_unsupported") {
		t.Fatalf("unsupported destination was claimed: %+v %v, diagnostic=%s", runs, err, api.OverrideComments[7])
	}
}

func TestInvalidHarnessOverridesRequireLabelCorrection(t *testing.T) {
	t.Parallel()

	for i, labels := range [][]string{
		{"mergeyard:implementer=gemini"},
		{"mergeyard:reviewer=claude", "MERGEYARD:REVIEWER=CODEX"},
		{"mergeyard:implementer=", "mergeyard:reviewer=unknown"},
	} {
		t.Run([]string{"unknown", "conflict", "both-invalid"}[i], func(t *testing.T) {
			t.Parallel()

			flow := testkit.NewScenario(t)
			_, _, api, _, cfg, r := flow.Parts()
			for _, label := range labels {
				api.Issues["owner/repo"][0].Labels = append(api.Issues["owner/repo"][0].Labels, github.Label{Name: label})
			}
			s, err := flow.New(cfg, scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			for range 3 {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			runs, err := s.Runs(context.Background())
			if err != nil || len(runs) != 0 || !testkit.Has(api.Issues["owner/repo"][0], cfg.Repositories[0].Labels.NeedsAttention) || len(api.OverrideComments) != 1 {
				t.Fatalf("invalid override claimed or unreported: %+v %v", runs, err)
			}
			comment := api.OverrideComments[7]
			for _, label := range labels {
				if !strings.Contains(comment, label) {
					t.Fatalf("comment missing %q: %s", label, comment)
				}
			}
			for _, value := range []string{"claude", "codex"} {
				if !strings.Contains(comment, value) {
					t.Fatal("comment missing valid values")
				}
			}
			// Corrections alone do not unblock the issue until attention is removed.
			api.Issues["owner/repo"][0].Labels = []github.Label{{Name: cfg.Repositories[0].Labels.Ready}, {Name: cfg.Repositories[0].Labels.NeedsAttention}, {Name: "mergeyard:implementer=claude"}, {Name: "MERGEYARD:IMPLEMENTER=CLAUDE"}}
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			runs, _ = s.Runs(context.Background())
			if len(runs) != 0 {
				t.Fatal("correction bypassed attention label")
			}
			if err := api.RemoveLabel(context.Background(), "owner/repo", 7, cfg.Repositories[0].Labels.NeedsAttention); err != nil {
				t.Fatal(err)
			}
			run := flow.Until(s, workflow.Active, workflow.Review)
			if run.RoleSettings.Implementer.HarnessSource != "label" || len(api.OverrideComments) != 1 {
				t.Fatal("corrected issue failed to claim once")
			}
		})
	}
}

func TestInvalidHarnessOverrideDeliverySurvivesRestart(t *testing.T) {
	t.Parallel()

	flow := testkit.NewScenario(t)
	_, _, api, _, cfg, r := flow.Parts()
	api.Issues["owner/repo"][0].Labels = append(api.Issues["owner/repo"][0].Labels, github.Label{Name: "mergeyard:implementer=unknown"})
	api.Mutate = func(action, _ string, _ int, label string) error {
		if action == "add" && label == cfg.Repositories[0].Labels.NeedsAttention {
			return errors.New("ambiguous label delivery")
		}
		return nil
	}
	s, err := flow.New(cfg, scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background()); err == nil {
		t.Fatal("expected interrupted label delivery")
	}
	api.Mutate = nil
	s, err = flow.New(cfg, scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := s.Runs(context.Background())
	if err != nil || len(runs) != 0 || len(api.OverrideComments) != 1 || !testkit.Has(api.Issues["owner/repo"][0], cfg.Repositories[0].Labels.NeedsAttention) {
		t.Fatal("refusal not recovered")
	}
}

func TestCorrectedHarnessOverridesCancelAmbiguousRefusalDelivery(t *testing.T) {
	t.Parallel()

	flow := testkit.NewScenario(t)
	_, _, api, _, cfg, r := flow.Parts()
	api.Issues["owner/repo"][0].Labels = append(api.Issues["owner/repo"][0].Labels, github.Label{Name: "mergeyard:implementer=unknown"})
	api.Mutate = func(action, _ string, _ int, label string) error {
		if action == "add" && label == cfg.Repositories[0].Labels.NeedsAttention {
			// GitHub committed the label, but the caller never received success.
			api.Issues["owner/repo"][0].Labels = append(api.Issues["owner/repo"][0].Labels, github.Label{Name: label})
			return errors.New("label response lost")
		}
		return nil
	}
	s, err := flow.New(cfg, scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background()); err == nil {
		t.Fatal("expected ambiguous delivery")
	}
	// Follow the comment while the daemon is offline: fix labels, remove attention.
	api.Mutate = nil
	api.Issues["owner/repo"][0].Labels = []github.Label{{Name: cfg.Repositories[0].Labels.Ready}, {Name: "mergeyard:implementer=claude"}}
	s, err = flow.New(cfg, scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(context.Background())
	if err != nil || len(runs) != 1 || testkit.Has(api.Issues["owner/repo"][0], cfg.Repositories[0].Labels.NeedsAttention) || len(api.OverrideComments) != 1 {
		t.Fatalf("stale refusal blocked corrected issue: runs=%d err=%v labels=%+v", len(runs), err, api.Issues["owner/repo"][0].Labels)
	}
}

func TestLegacyRunContinuesReadingRoleConfiguration(t *testing.T) {
	t.Parallel()

	flow := testkit.NewScenario(t)
	_, runtime, api, _, cfg, r := flow.Parts()
	_, err := runtime.Workflow.Transition(context.Background(), "legacy-run", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Repositories[0].Implementer.Model = "new-legacy-model"
	s, err := flow.New(cfg, scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := flow.Until(s, workflow.Active, workflow.Review)
	if run.ID != "legacy-run" || run.RoleSettings != nil || run.Implementer.Model != "new-legacy-model" {
		t.Fatalf("legacy settings: %+v", run)
	}
}
