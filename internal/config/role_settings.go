package config

import (
	"errors"
	"fmt"
	"strings"
)

// RoleSettings is the claim-time selection, independent of later configuration.
type RoleSettings struct {
	Agent         string   `json:"agent"`
	Model         string   `json:"model"`
	Effort        string   `json:"effort"`
	Skills        []string `json:"skills"`
	HarnessSource string   `json:"harness_source"`
}

type RunRoleSettings struct {
	Implementer RoleSettings `json:"implementer"`
	Reviewer    RoleSettings `json:"reviewer"`
}

// ResolveRoleSettings is shared by claiming and discovery diagnostics.
// Label spelling is fixed; model and effort belong to the configured harness.
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
	var overrides []string
	selected, invalid := "", false
	for _, label := range labels {
		if !strings.HasPrefix(strings.ToLower(label), prefix) {
			continue
		}
		overrides = append(overrides, label)
		agent := strings.ToLower(label[len(prefix):])
		if (agent != "claude" && agent != "codex") || (selected != "" && selected != agent) {
			invalid = true
		}
		selected = agent
	}
	if invalid {
		return RoleSettings{}, fmt.Errorf("Invalid %s harness override labels: %s. Valid values are %sclaude and %scodex. Fix the labels and remove the needs-attention label to allow another claim.", name, strings.Join(overrides, ", "), prefix, prefix)
	}
	if len(overrides) > 0 {
		settings.Agent, settings.HarnessSource = selected, "label"
		if selected != configured.Agent {
			settings.Model, settings.Effort = "", ""
		}
	}
	return settings, nil
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
