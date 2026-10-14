package config_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
)

func TestTierLabelsResolvePerRoleAndSelectedHarness(t *testing.T) {
	cfg, _, err := config.Parse([]byte(`
implementer:
  agent: claude
  model: ordinary-implement
  effort: medium
  skills: [implement]
  tiers:
    claude:
      high: {model: claude-high, effort: max}
    codex:
      high: {model: codex-high, effort: high}
reviewer:
  agent: claude
  model: ordinary-review
  tiers:
    claude:
      low: {model: claude-low, effort: medium}
repositories:
  - repo: owner/repo
`))
	if err != nil {
		t.Fatal(err)
	}
	settings, err := config.ResolveRoleSettings(cfg.Repositories[0], []string{
		"mergeyard:implementer=codex", "MERGEYARD:IMPLEMENTER-TIER=HIGH", "mergeyard:reviewer-tier=low",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := settings.Implementer; got.Agent != "codex" || got.Model != "codex-high" || got.Effort != "high" || len(got.Skills) != 1 || got.Skills[0] != "implement" {
		t.Fatalf("implementer tier did not use the selected harness: %+v", got)
	}
	if got := settings.Reviewer; got.Agent != "claude" || got.Model != "claude-low" || got.Effort != "medium" {
		t.Fatalf("reviewer tier did not resolve independently: %+v", got)
	}
	if settings.Implementer.Tier != "high" || !reflect.DeepEqual(settings.Implementer.TierLabels, []string{"MERGEYARD:IMPLEMENTER-TIER=HIGH"}) || settings.Reviewer.Tier != "low" {
		t.Fatalf("tier provenance: %+v", settings)
	}
}

func TestTierDefaultsReplaceOrdinarySettingsOnlyWhenLabelled(t *testing.T) {
	for _, fields := range []string{"{}", "{model: null, effort: null}", "{model: tier-model}", "{effort: high}"} {
		t.Run(fields, func(t *testing.T) {
			cfg, _, err := config.Parse([]byte("implementer:\n  model: ordinary\n  effort: max\n  tiers:\n    claude:\n      low: " + fields + "\nrepositories: [{repo: owner/repo}]"))
			if err != nil {
				t.Fatal(err)
			}
			plain, err := config.ResolveRoleSettings(cfg.Repositories[0], nil)
			if err != nil || plain.Implementer.Model != "ordinary" || plain.Implementer.Effort != "max" || plain.Implementer.Tier != "" || len(plain.Implementer.TierLabels) != 0 {
				t.Fatalf("unlabelled role changed: %+v %v", plain, err)
			}
			labelled, err := config.ResolveRoleSettings(cfg.Repositories[0], []string{"mergeyard:implementer-tier=low"})
			if err != nil {
				t.Fatal(err)
			}
			model, effort := "", ""
			if fields == "{model: tier-model}" {
				model = "tier-model"
			}
			if fields == "{effort: high}" {
				effort = "high"
			}
			if labelled.Implementer.Model != model || labelled.Implementer.Effort != effort || labelled.Reviewer.Tier != "" {
				t.Fatalf("tier inherited ordinary settings: %+v", labelled)
			}
		})
	}
}

func TestTierSelectionErrorsAndRepeatedLabels(t *testing.T) {
	repo := config.Repository{Implementer: config.Role{Agent: "claude", Tiers: map[string]map[string]config.TierSettings{"claude": {"low": {}}}}, Reviewer: config.Role{Agent: "codex"}}
	for _, tc := range []struct {
		name   string
		labels []string
		want   string
	}{
		{"conflict", []string{"mergeyard:implementer-tier=low", "mergeyard:implementer-tier=high"}, "Invalid implementer tier"},
		{"unknown", []string{"mergeyard:reviewer-tier=ultra"}, "Invalid reviewer tier"},
		{"missing tier", []string{"mergeyard:implementer-tier=high"}, "implementer.tiers.claude.high"},
		{"missing harness", []string{"mergeyard:implementer=codex", "mergeyard:implementer-tier=low"}, "implementer.tiers.codex.low"},
		{"no tier config", []string{"mergeyard:reviewer-tier=medium"}, "reviewer.tiers.codex.medium"},
		{"repeated", []string{"mergeyard:implementer-tier=low", "MERGEYARD:IMPLEMENTER-TIER=LOW"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings, err := config.ResolveRoleSettings(repo, tc.labels)
			if tc.want != "" {
				if err == nil || settings != nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("selection = %+v, %v; want %q", settings, err, tc.want)
				}
				for _, label := range tc.labels {
					if !strings.Contains(err.Error(), label) && !strings.Contains(label, "implementer=codex") {
						t.Fatalf("diagnostic omitted label %q: %v", label, err)
					}
				}
			} else if err != nil || !reflect.DeepEqual(settings.Implementer.TierLabels, []string{"MERGEYARD:IMPLEMENTER-TIER=LOW", "mergeyard:implementer-tier=low"}) {
				t.Fatalf("repeated selection: %+v %v", settings, err)
			}
		})
	}
}

func TestRepositoryTierMapsReplaceInheritedDefinitions(t *testing.T) {
	cfg, _, err := config.Parse([]byte(`
implementer:
  tiers:
    claude:
      low: {model: global-low}
      high: {model: global-high}
reviewer:
  tiers:
    codex:
      high: {}
repositories:
  - repo: owner/inherits
  - repo: owner/replaces
    implementer:
      tiers:
        codex:
          medium: {model: repo-medium}
  - repo: owner/clears
    implementer: {tiers: {}}
`))
	if err != nil {
		t.Fatal(err)
	}
	inherited, replacement, cleared := cfg.Repositories[0], cfg.Repositories[1], cfg.Repositories[2]
	if inherited.Implementer.Tiers["claude"]["low"].Model != "global-low" || replacement.Implementer.Tiers["codex"]["medium"].Model != "repo-medium" || len(replacement.Implementer.Tiers) != 1 || len(cleared.Implementer.Tiers) != 0 {
		t.Fatalf("tier map inheritance: %+v", cfg.Repositories)
	}
	if !reflect.DeepEqual(replacement.Reviewer.Tiers, cfg.Reviewer.Tiers) || !reflect.DeepEqual(cleared.Reviewer.Tiers, cfg.Reviewer.Tiers) {
		t.Fatal("implementer override changed reviewer tiers")
	}
	inherited.Implementer.Tiers["claude"]["low"] = config.TierSettings{Model: "edited"}
	if cfg.Implementer.Tiers["claude"]["low"].Model != "global-low" {
		t.Fatal("repository tier edits mutated global settings")
	}
}

func TestMalformedTierConfigurationAndUnknownFieldWarnings(t *testing.T) {
	for _, definition := range []string{
		"null", "[]", "{other: {low: {}}}", "{claude: {ultra: {}}}", "{claude: {low: null}}", "{claude: {low: []}}", "{claude: {low: {}, low: {}}}",
	} {
		t.Run(definition, func(t *testing.T) {
			_, _, err := config.Parse([]byte("implementer: {tiers: " + definition + "}"))
			if err == nil {
				t.Fatal("malformed tier configuration accepted")
			}
		})
	}
	_, doc, err := config.Parse([]byte("implementer: {tiers: {claude: {low: {effrot: medium}}}}"))
	if err != nil || len(doc.Warnings) != 1 || doc.Warnings[0].Path != "implementer.tiers.claude.low.effrot" {
		t.Fatalf("unknown tier field: %+v %v", doc, err)
	}
}

func TestRoleSettingsWithoutTierMetadataRemainUsable(t *testing.T) {
	var saved config.RunRoleSettings
	if err := json.Unmarshal([]byte(`{"implementer":{"agent":"codex","model":"saved-model","effort":"high","skills":["implement"],"harness_source":"label"},"reviewer":{"agent":"claude","model":"saved-review","harness_source":"config"}}`), &saved); err != nil {
		t.Fatal(err)
	}
	repo := saved.Apply(config.Repository{Implementer: config.Role{Agent: "claude", Model: "changed"}})
	if repo.Implementer.Agent != "codex" || repo.Implementer.Model != "saved-model" || repo.Reviewer.Model != "saved-review" || saved.Implementer.Tier != "" || len(saved.Implementer.TierLabels) != 0 {
		t.Fatalf("old snapshot changed: %+v %+v", saved, repo)
	}
}
