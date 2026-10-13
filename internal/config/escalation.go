package config

import (
	"strings"

	"github.com/rcpassos/mergeyard/internal/escalation"
)

func readEscalation(m mapping, rules *escalation.Rules) {
	if n := m.node("max_changed_lines"); n != nil {
		if n.Tag != "!!int" || n.Decode(&rules.MaxChangedLines) != nil || rules.MaxChangedLines < 0 {
			m.fail("config.invalid_escalation", "max_changed_lines", "expected a nonnegative finite integer; zero disables the size rule")
		}
	}
	m.read("new_dependencies", &rules.NewDependencies)
	if n := m.node("paths"); n != nil {
		paths := newMapping(n, m.fieldPath("paths"), m.err, m.warnings)
		rules.Paths = map[string]string{}
		for _, pattern := range paths.keys {
			var reason string
			if err := escalation.ValidateGlob(pattern); err != nil {
				paths.fail("config.invalid_escalation", pattern, err.Error())
			}
			node := paths.node(pattern)
			if node != nil && (node.Tag != "!!str" || node.Decode(&reason) != nil || strings.TrimSpace(reason) == "" || strings.ContainsAny(reason, "\x00\r\n")) {
				paths.fail("config.invalid_escalation", pattern, "expected a nonempty single-line reason")
			}
			rules.Paths[pattern] = reason
		}
		paths.finish()
	}
	m.finish()
}
