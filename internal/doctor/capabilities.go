package doctor

import (
	"regexp"

	"github.com/rcpassos/mergeyard/internal/config"
)

type assignedRole struct {
	scope string
	role  config.Role
}

func roles(cfg config.Config) []assignedRole {
	if len(cfg.Repositories) == 0 {
		return []assignedRole{{"implementer", cfg.Implementer}, {"reviewer", cfg.Reviewer}}
	}
	var result []assignedRole
	for _, repo := range cfg.Repositories {
		result = append(result, assignedRole{repo.Repo + " implementer", repo.Implementer}, assignedRole{repo.Repo + " reviewer", repo.Reviewer})
	}
	return result
}

func executable(cfg config.Config, agent string) string {
	if agent == "codex" {
		return cfg.Agents.Codex.Executable
	}
	return cfg.Agents.Claude.Executable
}

func (c *checker) capabilities(cfg config.Config) {
	help := make(map[string]string)
	failed := make(map[string]bool)
	for _, assigned := range roles(cfg) {
		role := assigned.role
		if (role.Model == "" && role.Effort == "") || !c.tools[role.Agent] {
			continue
		}
		if _, checked := help[role.Agent]; !checked && !failed[role.Agent] {
			result, err := c.command("", executable(cfg, role.Agent), "--help")
			if err != nil {
				failed[role.Agent] = true
			} else {
				help[role.Agent] = string(result.Stdout) + " " + string(result.Stderr)
			}
		}
		if failed[role.Agent] {
			c.report.add(Unverifiable, "harness.capabilities_unverifiable", assigned.scope, "cannot inspect harness help for requested model/effort capabilities")
			continue
		}
		effortFlag := "--effort"
		if role.Agent == "codex" {
			effortFlag = "--config"
		}
		for _, request := range []struct{ value, name, flag string }{{role.Model, "model", "--model"}, {role.Effort, "effort", effortFlag}} {
			if request.value != "" && !hasFlag(help[role.Agent], request.flag) {
				c.report.add(Error, "harness.capability_unsupported", assigned.scope, "harness does not advertise requested "+request.name+" selection ("+request.flag+")")
			}
		}
		// Both supported harnesses select skills through the prompt. Their
		// minimum versions establish this capability; locations are checked
		// separately. Model/effort values deliberately bypass any catalog.
	}
}

func hasFlag(help, flag string) bool {
	return regexp.MustCompile(`(?:^|[\s,])` + regexp.QuoteMeta(flag) + `(?:[\s=,]|$)`).MatchString(help)
}
