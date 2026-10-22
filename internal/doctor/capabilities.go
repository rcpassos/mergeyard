package doctor

import (
	"regexp"
	"strings"

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

func (c *checker) capabilities(cfg config.Config) {
	help := make(map[string]string)
	failed := make(map[string]bool)
	for _, assigned := range roles(cfg) {
		role := assigned.role
		facts, known := harnesses[role.Agent]
		if !known || (role.Model == "" && role.Effort == "") || !c.tools[role.Agent] {
			continue
		}
		if _, checked := help[role.Agent]; !checked && !failed[role.Agent] {
			result, err := c.command("", facts.executable(cfg), "--help")
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
		supported := true
		var requested []string
		for _, request := range []struct{ value, name, flag string }{{role.Model, "model", "--model"}, {role.Effort, "effort", facts.effortFlag}} {
			if request.value == "" {
				continue
			}
			requested = append(requested, request.name)
			if !hasFlag(help[role.Agent], request.flag) {
				supported = false
				c.report.add(Error, "harness.capability_unsupported", assigned.scope, "harness does not advertise requested "+request.name+" selection ("+request.flag+")")
			}
		}
		if supported {
			c.report.pass("harness.capabilities_supported", assigned.scope, "requested "+strings.Join(requested, " and ")+" selection is supported")
		}
		// Both supported harnesses select skills through the prompt. Their
		// minimum versions establish this capability; locations are checked
		// separately. Model/effort values deliberately bypass any catalog.
	}
}

func hasFlag(help, flag string) bool {
	return regexp.MustCompile(`(?:^|[\s,])` + regexp.QuoteMeta(flag) + `(?:[\s=,]|$)`).MatchString(help)
}
