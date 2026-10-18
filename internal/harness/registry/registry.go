// Package registry lists the coding-agent harnesses Mergeyard supports.
// Configuration validation, the doctor, the scheduler's adapters and the CLI
// read it, so the supported harnesses are named in one place.
package registry

import "slices"

// The supported harness names. They are also the configuration's agents
// sections, the harness override label values, and doctor finding scopes.
const (
	Claude = "claude"
	Codex  = "codex"
)

var names = []string{Claude, Codex}

// Names returns the supported harnesses in the order Mergeyard presents them.
func Names() []string { return slices.Clone(names) }

// Known reports whether name is a supported harness.
func Known(name string) bool { return slices.Contains(names, name) }
