package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/rcpassos/mergeyard/internal/harness/registry"
)

// RoleSettings is the claim-time selection, independent of later configuration.
type RoleSettings struct {
	Agent         string   `json:"agent"`
	Model         string   `json:"model"`
	Effort        string   `json:"effort"`
	Skills        []string `json:"skills"`
	HarnessSource string   `json:"harness_source"`
	Tier          string   `json:"tier,omitempty"`
	TierLabels    []string `json:"tier_labels,omitempty"`
}

type RunRoleSettings struct {
	Implementer RoleSettings `json:"implementer"`
	Reviewer    RoleSettings `json:"reviewer"`
}

// ResolveRoleSettings is shared by claiming and discovery diagnostics.
// Label spelling is fixed; tier settings belong to the selected harness.
func ResolveRoleSettings(repo Repository, labels []string) (*RunRoleSettings, error) {
	implementer, implementErr := resolveRole("implementer", repo.Implementer, labels)
	reviewer, reviewErr := resolveRole("reviewer", repo.Reviewer, labels)
	if err := errors.Join(implementErr, reviewErr); err != nil {
		return nil, err
	}
	return &RunRoleSettings{Implementer: implementer, Reviewer: reviewer}, nil
}

func resolveRole(name string, configured Role, labels []string) (RoleSettings, error) {
	settings := RoleSettings{Agent: configured.Agent, Model: configured.Model, Effort: configured.Effort, Skills: append([]string{}, configured.Skills...), HarnessSource: "config"}
	prefix := "mergeyard:" + name + "="
	selected, overrides, invalid := selectRoleLabel(labels, prefix, registry.Names()...)
	if invalid {
		valid := prefix + strings.Join(registry.Names(), " and "+prefix)
		return RoleSettings{}, fmt.Errorf("Invalid %s harness override labels: %s. Valid values are %s. Fix the labels and remove the needs-attention label to allow another claim.", name, strings.Join(overrides, ", "), valid)
	}
	if len(overrides) > 0 {
		settings.Agent, settings.HarnessSource = selected, "label"
		if selected != configured.Agent {
			settings.Model, settings.Effort = "", ""
		}
	}
	prefix = "mergeyard:" + name + "-tier="
	tier, matching, invalid := selectRoleLabel(labels, prefix, "low", "medium", "high")
	if invalid {
		return RoleSettings{}, fmt.Errorf("Invalid %s tier labels: %s. Valid values are %slow, %smedium and %shigh; select only one tier. Fix the labels and remove the needs-attention label to allow another claim.", name, strings.Join(matching, ", "), prefix, prefix, prefix)
	}
	if len(matching) > 0 {
		preset, ok := configured.Tiers[settings.Agent][tier]
		if !ok {
			return RoleSettings{}, fmt.Errorf("Undefined %s tier %q for %s, selected by labels: %s. Configure %s.tiers.%s.%s or correct the labels, then remove the needs-attention label to allow another claim.", name, tier, settings.Agent, strings.Join(matching, ", "), name, settings.Agent, tier)
		}
		settings.Model, settings.Effort = preset.Model, preset.Effort
		settings.Tier, settings.TierLabels = tier, matching
	}
	return settings, nil
}

func selectRoleLabel(labels []string, prefix string, choices ...string) (selected string, matching []string, invalid bool) {
	for _, label := range labels {
		if !strings.HasPrefix(strings.ToLower(label), prefix) {
			continue
		}
		matching = append(matching, label)
		value := strings.ToLower(label[len(prefix):])
		if !slices.Contains(choices, value) || (selected != "" && selected != value) {
			invalid = true
		}
		selected = value
	}
	slices.Sort(matching)
	return
}

// Apply preserves execution policy (such as max_attempts), freezing only the
// role settings promised by the run snapshot. Nil denotes a legacy run.
func (settings *RunRoleSettings) Apply(repo Repository) Repository {
	if settings == nil {
		return repo
	}
	apply := func(role Role, saved RoleSettings) Role {
		role.Agent, role.Model, role.Effort = saved.Agent, saved.Model, saved.Effort
		role.Skills = append([]string{}, saved.Skills...)
		return role
	}
	repo.Implementer = apply(repo.Implementer, settings.Implementer)
	repo.Reviewer = apply(repo.Reviewer, settings.Reviewer)
	return repo
}
