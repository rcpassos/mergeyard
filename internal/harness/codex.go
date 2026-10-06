package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// Codex adapts the native exec JSONL contract verified by the M2 live probes.
type Codex struct{ config config.Codex }

func NewCodex(cfg config.Codex) *Codex {
	if cfg.Executable == "" {
		cfg.Executable = "codex"
	}
	if cfg.Sandbox == "" {
		cfg.Sandbox = "workspace-write"
	}
	return &Codex{config: cfg}
}
func (*Codex) Type() string { return "codex" }
func (*Codex) Capabilities() HarnessCapabilities {
	return HarnessCapabilities{ModelSelection: true, EffortSelection: true, SkillSelection: true, StructuredOutput: true, SessionResume: true, SessionIDSource: Discovered}
}
func (c *Codex) ValidateConfig(role RoleConfig) error {
	if role.Agent != "" && role.Agent != c.Type() {
		return phaseError("config.invalid_agent", "Codex adapter requires agent: codex", nil)
	}
	if c.config.Sandbox != "workspace-write" && c.config.Sandbox != "danger-full-access" {
		return phaseError("config.invalid_sandbox", "Codex sandbox must be workspace-write or danger-full-access", nil)
	}
	if strings.TrimSpace(c.config.Executable) == "" || strings.ContainsRune(c.config.Executable, 0) {
		return phaseError("config.invalid_executable", "Codex executable must be nonempty and contain no NUL", nil)
	}
	for _, field := range []struct{ name, value string }{{"model", role.Model}, {"effort", role.Effort}} {
		if strings.ContainsRune(field.value, 0) {
			return phaseError("config.invalid_"+field.name, "Codex "+field.name+" must contain no NUL", nil)
		}
	}
	for _, skill := range role.Skills {
		if !skillName.MatchString(skill) || strings.Contains(skill, ":") {
			return phaseError("config.invalid_skills", "Codex skills must be names without whitespace, namespace, or a leading dollar sign", nil)
		}
	}
	return nil
}
func (c *Codex) BuildInvocation(ctx PhaseContext, role RoleConfig) (Invocation, error) {
	if err := c.ValidateConfig(role); err != nil {
		return Invocation{}, err
	}
	if err := validatePhaseContext(ctx); err != nil {
		return Invocation{}, err
	}

	if ctx.Resume && !sessionUUID.MatchString(ctx.SessionID) {
		return Invocation{}, phaseError("phase.invalid_request", "Codex resume requires an exact session UUID", nil)
	}
	args := []string{"exec", "--json", "-C", ctx.WorktreePath, "-s", c.config.Sandbox, "-c", `approval_policy="never"`, "-c", "sandbox_workspace_write.network_access=" + strconv.FormatBool(c.config.NetworkAccess), "--output-schema", filepath.Join(ctx.PhaseDir, "schema.json"), "-o", filepath.Join(ctx.PhaseDir, "last-message.json")}
	if role.Model != "" {
		args = append(args, "--model", role.Model)
	}
	if role.Effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+strconv.Quote(role.Effort))
	}
	var prompt strings.Builder
	for _, skill := range role.Skills {
		prompt.WriteString("$" + skill + " ")
	}
	prompt.WriteString("Read " + filepath.Join(ctx.PhaseDir, "input.md") + " and follow it. Do not commit.")
	if ctx.Resume {
		args = append(args, "resume", "--", ctx.SessionID, prompt.String())
	} else {
		args = append(args, "--", prompt.String())
	}
	return Invocation{Executable: c.config.Executable, Dir: ctx.WorktreePath, Env: ctx.Env, Args: args}, nil
}

// DiscoverSession reads complete JSONL records from a live capture. The final
// partial line is ignored until the writer finishes it; malformed records fail.
func (*Codex) DiscoverSession(data []byte) (string, error) {
	for _, line := range bytes.SplitAfter(data, []byte("\n")) {
		if len(line) == 0 || line[len(line)-1] != '\n' {
			break
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event codexEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return "", phaseError("phase.result_invalid", "Codex event is not valid JSON", err)
		}
		if event.Type != "thread.started" {
			continue
		}
		if !sessionUUID.MatchString(event.ThreadID) {
			return "", phaseError("harness.session_identity_invalid", "Codex emitted an invalid thread identity", nil)
		}
		// Save the first identity even if later output is malformed or truncated.
		// ParseResult validates the entire completed stream, including conflicts.
		return event.ThreadID, nil
	}
	return "", nil
}

type codexEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"`
	Error    struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (*Codex) ParseResult(ctx PhaseContext, artifacts PhaseArtifacts) (PhaseResult, error) {
	if ctx.Phase != workflow.Implement && ctx.Phase != workflow.Review && ctx.Phase != workflow.Fix {
		return PhaseResult{}, phaseError("phase.unsupported", "Only implement, review, and fix phases are supported", nil)
	}
	diagnostic := strings.TrimSpace(string(artifacts.Stderr))
	if ctx.Resume && artifacts.ExitCode != 0 && matchesExactDiagnostic(artifacts.Stderr, "Error: thread/resume: thread/resume failed: no rollout found for thread id "+ctx.SessionID+" (code -32600)") {
		return PhaseResult{}, phaseError("harness.session_resume_failed", diagnostic, nil)
	}
	if artifacts.ExitCode != 0 {
		// Provider errors can be native events with an empty stderr. Keep their
		// messages while rejecting the process regardless of any apparent result.
		decoder := json.NewDecoder(bytes.NewReader(artifacts.Stdout))
		for {
			var event codexEvent
			if decoder.Decode(&event) != nil {
				break
			}
			if event.Type == "error" || event.Type == "turn.failed" {
				message := event.Message + event.Error.Message
				if message != "" {
					diagnostic += "\n" + message
				}
			}
		}
		return PhaseResult{}, phaseError("phase.execution_failed", fmt.Sprintf("Codex exited with code %d: %s", artifacts.ExitCode, strings.TrimSpace(diagnostic)), nil)
	}
	decoder := json.NewDecoder(bytes.NewReader(artifacts.Stdout))
	var identity string
	completed := false
	for {
		var event codexEvent
		if err := decoder.Decode(&event); err == io.EOF {
			break
		} else if err != nil {
			return PhaseResult{}, phaseError("phase.result_invalid", "Codex output is not valid JSON", err)
		}
		switch event.Type {
		case "thread.started":
			if !sessionUUID.MatchString(event.ThreadID) || (identity != "" && identity != event.ThreadID) {
				return PhaseResult{}, phaseError("harness.session_identity_invalid", "Codex emitted an invalid or conflicting thread identity", nil)
			}
			identity = event.ThreadID
		case "turn.completed":
			completed = true
		case "turn.failed", "error":
			return PhaseResult{}, phaseError("phase.execution_failed", "Codex failed: "+event.Message+event.Error.Message, nil)
		}
	}
	if identity == "" {
		return PhaseResult{}, phaseError("harness.session_identity_missing", "Codex did not emit a thread identity", nil)
	}
	if ctx.SessionID != "" && identity != ctx.SessionID {
		return PhaseResult{}, phaseError("harness.session_identity_invalid", "Codex returned a different thread than the requested session", nil)
	}
	if !completed || len(bytes.TrimSpace(artifacts.LastMessage)) == 0 {
		return PhaseResult{}, phaseError("phase.result_missing", "Codex did not emit native completion and a final structured message", nil)
	}
	return parsePhaseResult(ctx.Phase, artifacts.LastMessage)
}

var _ HarnessAdapter = (*Codex)(nil)
