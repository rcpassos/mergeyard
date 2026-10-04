package doctor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/rcpassos/mergeyard/internal/config"
)

var skillName = regexp.MustCompile(`^[A-Za-z0-9_-]+(?::[A-Za-z0-9_-]+)?$`)

func repositorySkillPath(agent, skill string) string {
	dir := ".claude"
	if agent == "codex" {
		dir = ".agents"
	}
	return dir + "/skills/" + skill + "/SKILL.md"
}

func (c *checker) skills(role config.Role, scope string, files map[string]repositoryFileType) {
	for _, skill := range role.Skills {
		if !skillName.MatchString(skill) || (role.Agent != "claude" && strings.Contains(skill, ":")) {
			c.report.add(Error, "harness.skill_invalid", scope, fmt.Sprintf("%q is not a supported skill name", skill))
			continue
		}
		if strings.Contains(skill, ":") {
			c.report.add(Unverifiable, "harness.skill_unverifiable", scope, fmt.Sprintf("Claude plugin skill %q can only be confirmed in the runtime system/init event", skill))
			continue
		}
		projectType := files[repositorySkillPath(role.Agent, skill)]
		if projectType == fileRegular {
			continue
		}
		roots, err := personalSkillRoots(role.Agent)
		if err != nil {
			c.report.add(Unverifiable, "harness.skill_unverifiable", scope, fmt.Sprintf("cannot resolve skill %q locations: %v", skill, err))
			continue
		}
		found := false
		var accessErr error
		for _, root := range roots {
			path := filepath.Join(root, skill, "SKILL.md")
			info, err := os.Stat(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				accessErr = err
				continue
			}
			if !info.Mode().IsRegular() {
				continue
			}
			file, err := os.Open(path)
			if err != nil {
				accessErr = err
				continue
			}
			if err := file.Close(); err != nil {
				accessErr = err
				continue
			}
			found = true
			break
		}
		if found {
			continue
		}
		if accessErr != nil || files == nil || projectType == fileUnverifiable {
			c.report.add(Unverifiable, "harness.skill_unverifiable", scope, fmt.Sprintf("cannot inspect every location for skill %q", skill))
		} else {
			c.report.add(Error, "harness.skill_missing", scope, fmt.Sprintf("skill %q has no SKILL.md in repository or personal/system %s skill locations", skill, role.Agent))
		}
	}
}

func personalSkillRoots(agent string) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	if agent == "claude" {
		root := os.Getenv("CLAUDE_CONFIG_DIR")
		if root == "" {
			root = filepath.Join(home, ".claude")
		}
		root, err := expandHome(root)
		return []string{filepath.Join(root, "skills")}, err
	}
	// Codex's personal skills live in ~/.agents, independent of CODEX_HOME.
	// Bundled system skills are also available under CODEX_HOME/skills/.system.
	root := os.Getenv("CODEX_HOME")
	if root == "" {
		root = filepath.Join(home, ".codex")
	}
	root, err = expandHome(root)
	return []string{filepath.Join(home, ".agents", "skills"), "/etc/codex/skills", filepath.Join(root, "skills", ".system")}, err
}
