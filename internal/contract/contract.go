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

// Field names one required top-level property of a phase result. Callers name
// fields through these constants so a typo or rename fails to compile.
type Field string

const (
	SchemaVersion Field = "schema_version"
	Status        Field = "status"
	Summary       Field = "summary"
	HumanAction   Field = "human_action"
	Criteria      Field = "criteria"
	Verification  Field = "verification"
	Findings      Field = "findings"
	PriorFindings Field = "prior_findings"
	Responses     Field = "responses"
)

// VersionToken marks the current-version slot in a schema template. A constant
// integer cannot be concatenated into a string constant, so schemas are built
// by binding the token at start-up.
const VersionToken = "{{version}}"

// Requirement is the exact set of top-level fields one schema version requires.
type Requirement struct {
	Fields []Field
}

// Requires reports whether the version's contract includes the named field.
func (r Requirement) Requires(field Field) bool { return slices.Contains(r.Fields, field) }

// Matches reports whether a decoded result carries exactly the required fields:
// none missing, and because additionalProperties is false, none extra. A field
// present with a null value still matches; type checks own nullability.
func (r Requirement) Matches(fields map[string]json.RawMessage) bool {
	if len(fields) != len(r.Fields) {
		return false
	}
	for _, field := range r.Fields {
		if fields[string(field)] == nil {
			return false
		}
	}
	return true
}

// String lists the required fields for error messages.
func (r Requirement) String() string {
	names := make([]string, len(r.Fields))
	for i, field := range r.Fields {
		names[i] = string(field)
	}
	return strings.Join(names, ", ")
}

// versions holds one ordered table per phase: index i describes version i+1.
var versions = map[Phase][]Requirement{
	Implement: {
		{Fields: []Field{SchemaVersion, Status, Summary}},
		{Fields: []Field{SchemaVersion, Status, Summary, HumanAction}},
		{Fields: []Field{SchemaVersion, Status, Summary, HumanAction, Criteria}},
		{Fields: []Field{SchemaVersion, Status, Summary, HumanAction, Criteria, Verification}},
	},
	Review: {
		{Fields: []Field{SchemaVersion, Status, Summary, Findings}},
		{Fields: []Field{SchemaVersion, Status, Summary, Findings, PriorFindings}},
		{Fields: []Field{SchemaVersion, Status, Summary, Findings, PriorFindings, HumanAction}},
		{Fields: []Field{SchemaVersion, Status, Summary, Findings, PriorFindings, HumanAction, Criteria}},
	},
	Fix: {
		{Fields: []Field{SchemaVersion, Status, Summary, Responses}},
		{Fields: []Field{SchemaVersion, Status, Summary, Responses, HumanAction}},
		{Fields: []Field{SchemaVersion, Status, Summary, Responses, HumanAction, Criteria}},
		{Fields: []Field{SchemaVersion, Status, Summary, Responses, HumanAction, Criteria, Verification}},
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
