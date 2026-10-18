// Package harness adapts coding-agent CLIs (Claude Code, Codex) to Mergeyard phases.
package harness

import (
	"encoding/json"
	"time"

	"github.com/rcpassos/mergeyard/internal/acceptance"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/contract"
	"github.com/rcpassos/mergeyard/internal/execution"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/review"
	"github.com/rcpassos/mergeyard/internal/verification"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// HarnessAdapter translates phase contracts without owning process execution.
type HarnessAdapter interface {
	Type() string
	Capabilities() HarnessCapabilities
	ValidateConfig(RoleConfig) error
	// BuildInvocation returns the phase command together with the effective
	// permissions the adapter applies to it, so attempts record what runs.
	BuildInvocation(PhaseContext, RoleConfig) (PhaseInvocation, error)
	// ResultArtifacts names the adapter-owned files a completed attempt must
	// capture for ParseResult, relative to the attempt directory.
	ResultArtifacts() []string
	BuildInteractiveInvocation(sessionID, worktree string) (InteractiveCommand, error)
	BuildCheckInvocation(dir string, env map[string]string) (Invocation, error)
	ParseResult(PhaseContext, PhaseArtifacts) (PhaseResult, error)
	// NativeSucceeded is adapter-owned completion evidence, independent of the task report.
	NativeSucceeded(PhaseArtifacts) bool
	ClassifyFailure(PhaseArtifacts, time.Time) FailureClassification
}

// Permissions is one adapter's effective permission surface for a built
// command. Summary is the attempt's audit record. Mode and AllowedTools are
// the values recorded with review and fix rows: the harness-native permission
// mode and tool list where the harness has them, and the command's sandbox and
// network surface where it does not.
type Permissions struct {
	Summary      string
	Mode         string
	AllowedTools []string
}

// PhaseInvocation is one adapter-built command and the permissions the adapter
// applies to it. Building the two together is what keeps each attempt's
// recorded permissions from drifting from the command that ran.
type PhaseInvocation struct {
	Command     Invocation
	Permissions Permissions
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
	// ResultSchemaVersion comes from the attempt's persisted schema on restart.
	ResultSchemaVersion int
	// AllowLegacyReview preserves the original v1 review restart contract.
	AllowLegacyReview bool
	Interruption      string
	Phase             workflow.Phase
	WorktreePath      string
	PhaseDir          string
	SessionID         string
	Resume            bool
	Env               map[string]string
}

// PhaseArtifacts is a completed attempt's captured stream and process exit.
// Live or incomplete logs must not be passed as a completed attempt.
type PhaseArtifacts struct {
	// StartedAt bounds the capture date for date-less native diagnostics. A zero
	// value means the execution date is unavailable; observation time alone is
	// insufficient when replaying artifacts after a control-plane restart.
	StartedAt time.Time
	// Files holds the adapter's ResultArtifacts, keyed by the reported name.
	Files    map[string][]byte
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// PhaseResult is the schema-validated report. Blocked and failed reports are
// valid results; the workflow decides their lifecycle consequences.
type PhaseResult struct {
	Verification []verification.Entry `json:"verification"`
	// Criteria is the phase-specific array, validated before translation.
	Criteria      json.RawMessage       `json:"criteria,omitempty"`
	HumanAction   *fault.HumanAction    `json:"human_action"`
	SchemaVersion int                   `json:"schema_version"`
	Status        string                `json:"status"`
	Summary       string                `json:"summary"`
	Responses     []review.Response     `json:"responses,omitempty"`
	Findings      []review.Finding      `json:"findings,omitempty"`
	PriorFindings []review.PriorFinding `json:"prior_findings,omitempty"`
}

// ImplementSchema returns a fresh copy of the common OpenAI strict schema.
func ImplementSchema() json.RawMessage { return json.RawMessage(implementSchema) }

// implementSchema is the strict implement contract for the current version.
var implementSchema = contract.Bind(implementSchemaTemplate)

const implementSchemaTemplate = `{"type":"object","properties":{"schema_version":{"type":"integer","const":` + contract.VersionToken + `},"status":{"type":"string","enum":["success","blocked","failed"]},"summary":{"type":"string"},"human_action":` + fault.HumanActionSchema + `,"criteria":` + acceptance.EvidenceSchema + `,"verification":` + verification.Schema + `},"required":["schema_version","status","summary","human_action","criteria","verification"],"additionalProperties":false}`
