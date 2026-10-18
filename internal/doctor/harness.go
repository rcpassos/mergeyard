package doctor

import (
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/harness/registry"
)

// harnessFacts are the harness-specific probes the doctor runs. Every harness
// in the registry has one entry, kept in step by
// TestEveryRegisteredHarnessHasDoctorFacts: without it the doctor would probe a
// new harness with another harness's executable, version minimum or login
// check instead of reporting that it cannot check it.
type harnessFacts struct {
	// executable resolves the configured binary for this harness.
	executable func(config.Config) string
	minimum    version
	auth       []string
	effortFlag string
	// skillDir is where the harness reads repository skills.
	skillDir string
	// namespacedSkills reports whether skills may carry a namespace, as Claude
	// plugin skills do.
	namespacedSkills bool
}

// repositorySkillPath is where the harness reads one repository skill.
func (f harnessFacts) repositorySkillPath(skill string) string {
	return f.skillDir + "/skills/" + skill + "/SKILL.md"
}

var harnesses = map[string]harnessFacts{
	registry.Claude: {
		executable:       func(cfg config.Config) string { return cfg.Agents.Claude.Executable },
		minimum:          claudeMinimum,
		auth:             []string{"auth", "status"},
		effortFlag:       "--effort",
		skillDir:         ".claude",
		namespacedSkills: true,
	},
	registry.Codex: {
		executable: func(cfg config.Config) string { return cfg.Agents.Codex.Executable },
		minimum:    codexMinimum,
		auth:       []string{"login", "status"},
		effortFlag: "--config",
		skillDir:   ".agents",
	},
}
