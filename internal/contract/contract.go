// Package contract owns the phase result schema versions shared by the
// parsers, the output schemas, and durable result validation: one
// current-version constant and one version table per phase.
package contract

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
)

// CurrentVersion is the schema version every new attempt writes. Restored
// attempts keep the version they were launched with.
const CurrentVersion = 4

// Phase names one phase result contract.
type Phase string

const (
	Implement Phase = "implement"
	Review    Phase = "review"
	Fix       Phase = "fix"
)

// VersionToken marks the current-version slot in a schema template. A constant
// integer cannot be concatenated into a string constant, so schemas are built
// by binding the token at start-up.
const VersionToken = "{{version}}"

// Requirement is the exact set of top-level fields one schema version requires.
type Requirement struct {
	Fields []string
}

// Requires reports whether the version's contract includes the named field.
func (r Requirement) Requires(field string) bool { return slices.Contains(r.Fields, field) }

// Matches reports whether a decoded result carries exactly the required fields:
// none missing, and because additionalProperties is false, none extra. A field
// present with a null value still matches; type checks own nullability.
func (r Requirement) Matches(fields map[string]json.RawMessage) bool {
	if len(fields) != len(r.Fields) {
		return false
	}
	for _, field := range r.Fields {
		if fields[field] == nil {
			return false
		}
	}
	return true
}

// String lists the required fields for error messages.
func (r Requirement) String() string { return strings.Join(r.Fields, ", ") }

// versions holds one ordered table per phase: index i describes version i+1.
var versions = map[Phase][]Requirement{
	Implement: {
		{Fields: []string{"schema_version", "status", "summary"}},
		{Fields: []string{"schema_version", "status", "summary", "human_action"}},
		{Fields: []string{"schema_version", "status", "summary", "human_action", "criteria"}},
		{Fields: []string{"schema_version", "status", "summary", "human_action", "criteria", "verification"}},
	},
	Review: {
		{Fields: []string{"schema_version", "status", "summary", "findings"}},
		{Fields: []string{"schema_version", "status", "summary", "findings", "prior_findings"}},
		{Fields: []string{"schema_version", "status", "summary", "findings", "prior_findings", "human_action"}},
		{Fields: []string{"schema_version", "status", "summary", "findings", "prior_findings", "human_action", "criteria"}},
	},
	Fix: {
		{Fields: []string{"schema_version", "status", "summary", "responses"}},
		{Fields: []string{"schema_version", "status", "summary", "responses", "human_action"}},
		{Fields: []string{"schema_version", "status", "summary", "responses", "human_action", "criteria"}},
		{Fields: []string{"schema_version", "status", "summary", "responses", "human_action", "criteria", "verification"}},
	},
}

// For returns one phase version's requirements and whether that version exists.
// Everything version-dependent, including whether the durable criteria file is
// required, reads from these tables.
func For(phase Phase, version int) (Requirement, bool) {
	table := versions[phase]
	if version < 1 || version > len(table) {
		return Requirement{}, false
	}
	return table[version-1], true
}

// Bind replaces the version token in a schema template with CurrentVersion.
func Bind(template string) string {
	return strings.Replace(template, VersionToken, strconv.Itoa(CurrentVersion), 1)
}
