package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// Claude adapts Claude Code's print mode. It never launches a process.
type Claude struct{ config config.Claude }

func NewClaude(cfg config.Claude) *Claude {
	if cfg.Executable == "" {
		cfg.Executable = "claude"
	}
	if cfg.PermissionMode == "" {
		cfg.PermissionMode = "bypassPermissions"
	}
	return &Claude{config: cfg}
}

func (*Claude) Type() string { return "claude" }

func (*Claude) Capabilities() HarnessCapabilities {
	return HarnessCapabilities{ModelSelection: true, EffortSelection: true, SkillSelection: true,
		StructuredOutput: true, SessionResume: true, SessionIDSource: Preassigned}
}

var skillName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
var sessionUUID = regexp.MustCompile(`^[[:xdigit:]]{8}-[[:xdigit:]]{4}-[[:xdigit:]]{4}-[[:xdigit:]]{4}-[[:xdigit:]]{12}$`)

func (c *Claude) ValidateConfig(role RoleConfig) error {
	if role.Agent != "" && role.Agent != c.Type() {
		return phaseError("config.invalid_agent", "Claude adapter requires agent: claude", nil)
	}
	if c.config.PermissionMode != "auto" && c.config.PermissionMode != "acceptEdits" && c.config.PermissionMode != "bypassPermissions" {
		return phaseError("config.invalid_permission_mode", "Claude permission mode must be auto, acceptEdits, or bypassPermissions", nil)
	}
	if strings.TrimSpace(c.config.Executable) == "" || strings.ContainsRune(c.config.Executable, 0) {
		return phaseError("config.invalid_executable", "Claude executable must be nonempty and contain no NUL", nil)
	}
	for _, field := range []struct{ name, value string }{{"model", role.Model}, {"effort", role.Effort}} {
		if strings.ContainsRune(field.value, 0) {
			return phaseError("config.invalid_"+field.name, "Claude "+field.name+" must contain no NUL", nil)
		}
	}
	for _, skill := range role.Skills {
		if !skillName.MatchString(skill) {
			return phaseError("config.invalid_skills", "Claude skills must be names without a leading slash or whitespace", nil)
		}
	}
	for _, tool := range c.config.AllowedTools {
		if strings.TrimSpace(tool) == "" || strings.HasPrefix(tool, "-") || strings.ContainsRune(tool, 0) {
			return phaseError("config.invalid_allowed_tools", "Claude allow rules must be nonempty tool rules, not CLI flags", nil)
		}
	}
	return nil
}

func (c *Claude) BuildInvocation(ctx PhaseContext, role RoleConfig) (Invocation, error) {
	if err := c.ValidateConfig(role); err != nil {
		return Invocation{}, err
	}
	if err := validateImplementContext(ctx); err != nil {
		return Invocation{}, err
	}
	if !sessionUUID.MatchString(ctx.SessionID) {
		return Invocation{}, phaseError("phase.invalid_request", "Claude requires a preassigned session UUID", nil)
	}
	sessionFlag := "--session-id"
	if ctx.Resume {
		sessionFlag = "--resume"
	}
	args := []string{"-p", sessionFlag, ctx.SessionID, "--output-format", "stream-json", "--verbose", "--json-schema", implementSchema}
	if len(c.config.AllowedTools) > 0 {
		args = append(args, "--allowedTools")
		args = append(args, c.config.AllowedTools...)
	}
	args = append(args, "--permission-mode", c.config.PermissionMode, "--add-dir", ctx.PhaseDir)
	if role.Model != "" {
		args = append(args, "--model", role.Model)
	}
	if role.Effort != "" {
		args = append(args, "--effort", role.Effort)
	}
	var prompt strings.Builder
	for _, skill := range role.Skills {
		prompt.WriteString("/" + skill + " ")
	}
	prompt.WriteString("Read " + filepath.Join(ctx.PhaseDir, "input.md") + " and follow it. Do not commit.")
	// End variadic CLI options before the positional prompt.
	args = append(args, "--", prompt.String())
	return Invocation{Executable: c.config.Executable, Dir: ctx.WorktreePath, Env: ctx.Env, Args: args}, nil
}

func (*Claude) ParseResult(ctx PhaseContext, artifacts PhaseArtifacts) (PhaseResult, error) {
	if ctx.Phase != workflow.Implement {
		return PhaseResult{}, phaseError("phase.unsupported", "Only the implement phase is supported", nil)
	}
	const resumeFailure = "No conversation found with session ID"
	if ctx.Resume && artifacts.ExitCode != 0 &&
		(strings.Contains(string(artifacts.Stderr), resumeFailure) || strings.HasPrefix(strings.TrimSpace(string(artifacts.Stdout)), resumeFailure)) {
		return PhaseResult{}, phaseError("harness.session_resume_failed", "Claude could not resume the requested session", nil)
	}
	decoder := json.NewDecoder(bytes.NewReader(artifacts.Stdout))
	var last json.RawMessage
	for {
		var event struct {
			Type string `json:"type"`
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err == io.EOF {
			break
		} else if err != nil {
			return PhaseResult{}, phaseError("phase.result_invalid", "Harness output is not valid JSON", err)
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			return PhaseResult{}, phaseError("phase.result_invalid", "Harness event is not an object", err)
		}
		if event.Type == "result" {
			last = raw
		}
	}
	if last == nil {
		if artifacts.ExitCode != 0 {
			return PhaseResult{}, phaseError("phase.execution_failed", fmt.Sprintf("Harness exited with code %d", artifacts.ExitCode), nil)
		}
		return PhaseResult{}, phaseError("phase.result_missing", "Harness did not emit a result event", nil)
	}
	var event struct {
		IsError *bool           `json:"is_error"`
		Subtype string          `json:"subtype"`
		Output  json.RawMessage `json:"structured_output"`
		Result  string          `json:"result"`
		Errors  []string        `json:"errors"`
	}
	if err := json.Unmarshal(last, &event); err != nil {
		return PhaseResult{}, phaseError("phase.result_invalid", "Harness result is invalid", err)
	}
	if ctx.Resume && (artifacts.ExitCode != 0 || (event.IsError != nil && *event.IsError)) &&
		strings.Contains(event.Result+"\n"+strings.Join(event.Errors, "\n"), resumeFailure) {
		return PhaseResult{}, phaseError("harness.session_resume_failed", "Claude could not resume the requested session", nil)
	}
	switch event.Subtype {
	case "error_max_structured_output_retries":
		return PhaseResult{}, phaseError("phase.result_invalid", "Harness exhausted structured output retries", nil)
	case "error_max_turns":
		return PhaseResult{}, phaseError("phase.max_turns_exceeded", "Harness reached its turn limit", nil)
	case "error_max_budget_usd":
		return PhaseResult{}, phaseError("phase.budget_exceeded", "Harness reached its budget limit", nil)
	}
	if artifacts.ExitCode != 0 || (event.IsError != nil && *event.IsError) || (event.Subtype != "" && event.Subtype != "success") {
		return PhaseResult{}, phaseError("phase.execution_failed", fmt.Sprintf("Harness failed (exit %d, subtype %q)", artifacts.ExitCode, event.Subtype), nil)
	}
	if event.IsError == nil {
		return PhaseResult{}, phaseError("phase.result_invalid", "Harness result must contain is_error: false", nil)
	}
	if len(event.Output) == 0 || bytes.Equal(event.Output, []byte("null")) {
		return PhaseResult{}, phaseError("phase.result_missing", "Harness did not provide structured output", nil)
	}
	return parseImplementResult(event.Output)
}

func phaseError(code, message string, cause error) *fault.Error {
	return &fault.Error{Code: code, Message: message, Err: cause}
}

var _ HarnessAdapter = (*Claude)(nil)
