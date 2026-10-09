// Package harness adapts coding-agent CLIs (Claude Code, Codex) to Mergeyard phases.
package harness

import (
	"encoding/json"
	"time"

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
	BuildInteractiveInvocation(sessionID, worktree string) (InteractiveCommand, error)
	BuildCheckInvocation(dir string, env map[string]string) (Invocation, error)
	ParseResult(PhaseContext, PhaseArtifacts) (PhaseResult, error)
	// NativeSucceeded is adapter-owned completion evidence, independent of the task report.
	NativeSucceeded(PhaseArtifacts) bool
	ClassifyFailure(PhaseArtifacts, time.Time) FailureClassification
}

// CheckConfigurator inspects effective local configuration without a model call.
// The runner executes the inspection; adapters interpret it and disable tools.
type CheckConfigurator interface {
	CheckConfigurationInvocation(Invocation) Invocation
	ConfigureCheckInvocation(Invocation, []byte) (Invocation, error)
}

// SessionDiscoverer reads early identity from complete records in a live stream.
type SessionDiscoverer interface {
	DiscoverSession([]byte) (string, error)
}

type SessionIDSource string

const (
	Preassigned SessionIDSource = "preassigned"
	Discovered  SessionIDSource = "discovered"
)

type HarnessCapabilities struct {
	ModelSelection            bool
	EffortSelection           bool
	SkillSelection            bool
	StructuredOutput          bool
	SessionResume             bool
	TemporaryLimitDetection   bool
	CreditExhaustionDetection bool
	SessionIDSource           SessionIDSource
}

type RoleConfig = config.Role
type Invocation = execution.Command

// PhaseContext contains only control-plane values. SessionID must be persisted
// before launching a preassigned session. Env is the complete child environment.
// PhaseDir is an existing attempt directory outside the worktree.
type PhaseContext struct {
	Interruption string
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
	// LastMessage is the native Codex -o artifact from this unique attempt.
	LastMessage []byte
	Stdout      []byte
	Stderr      []byte
	ExitCode    int
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
