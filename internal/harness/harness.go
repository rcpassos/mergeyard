// Package harness adapts coding-agent CLIs (Claude Code, Codex) to Mergeyard phases.
package harness

import (
	"encoding/json"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/execution"
	"github.com/rcpassos/mergeyard/internal/review"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// HarnessAdapter translates phase contracts without owning process execution.
type HarnessAdapter interface {
	Type() string
	Capabilities() HarnessCapabilities
	ValidateConfig(RoleConfig) error
	BuildInvocation(PhaseContext, RoleConfig) (Invocation, error)
	ParseResult(PhaseContext, PhaseArtifacts) (PhaseResult, error)
}

type SessionIDSource string

const (
	Preassigned SessionIDSource = "preassigned"
	Discovered  SessionIDSource = "discovered"
)

type HarnessCapabilities struct {
	ModelSelection      bool
	EffortSelection     bool
	SkillSelection      bool
	StructuredOutput    bool
	SessionResume       bool
	UsageLimitDetection bool
	SessionIDSource     SessionIDSource
}

type RoleConfig = config.Role
type Invocation = execution.Command

// PhaseContext contains only control-plane values. SessionID must be persisted
// before launching a preassigned session. Env is the complete child environment.
// PhaseDir is an existing attempt directory outside the worktree.
type PhaseContext struct {
	Phase        workflow.Phase
	WorktreePath string
	PhaseDir     string
	SessionID    string
	Resume       bool
	Env          map[string]string
}

// PhaseArtifacts is a completed attempt's captured stream and process exit.
// Live or incomplete logs must not be passed as a completed attempt.
type PhaseArtifacts struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// PhaseResult is the schema-validated report. Blocked and failed reports are
// valid results; the workflow decides their lifecycle consequences.
type PhaseResult struct {
	SchemaVersion int               `json:"schema_version"`
	Status        string            `json:"status"`
	Summary       string            `json:"summary"`
	Responses     []review.Response `json:"responses,omitempty"`
	Findings      []review.Finding  `json:"findings,omitempty"`
}

// ImplementSchema returns a fresh copy of the common OpenAI strict schema.
func ImplementSchema() json.RawMessage { return json.RawMessage(implementSchema) }

const implementSchema = `{"type":"object","properties":{"schema_version":{"type":"integer","const":1},"status":{"type":"string","enum":["success","blocked","failed"]},"summary":{"type":"string"}},"required":["schema_version","status","summary"],"additionalProperties":false}`
