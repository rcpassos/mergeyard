package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

const sessionID = "7f9e8221-470e-43e0-adc7-41798c5c193e"

const successEvent = `{"type":"result","subtype":"success","is_error":false,"structured_output":{"schema_version":1,"status":"success","summary":"Implemented the issue"}}`

func phase(t *testing.T) harness.PhaseContext {
	t.Helper()
	return harness.PhaseContext{Phase: workflow.Implement, WorktreePath: t.TempDir(), PhaseDir: t.TempDir(), SessionID: sessionID}
}

func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	var failure *fault.Error
	if !errors.As(err, &failure) || failure.Code != code || failure.Message == "" {
		t.Fatalf("error = %v; want %s with a human message", err, code)
	}
}

func TestClaudeRejectsIncompleteOrInvalidResults(t *testing.T) {
	cases := []struct {
		name, output, exit, code string
	}{
		{"no completion", `{"type":"system"}`, "0", "phase.result_missing"},
		{"missing structured output", `{"type":"result","is_error":false,"result":"prose is not a result"}`, "0", "phase.result_missing"},
		{"null output", `{"type":"result","is_error":false,"structured_output":null}`, "0", "phase.result_missing"},
		{"invalid JSON", `{not JSON`, "0", "phase.result_invalid"},
		{"nonzero exit", successEvent, "7", "phase.execution_failed"},
		{"nonzero without result", "", "7", "phase.execution_failed"},
		{"error event", `{"type":"result","is_error":true,"subtype":"error_during_execution"}`, "0", "phase.execution_failed"},
		{"missing completion flag", `{"type":"result","structured_output":{"schema_version":1,"status":"success","summary":"done"}}`, "0", "phase.result_invalid"},
		{"schema retries exhausted", `{"type":"result","is_error":true,"subtype":"error_max_structured_output_retries"}`, "1", "phase.result_invalid"},
		{"turns exhausted", `{"type":"result","is_error":true,"subtype":"error_max_turns"}`, "1", "phase.max_turns_exceeded"},
		{"budget exhausted", `{"type":"result","is_error":true,"subtype":"error_max_budget_usd"}`, "1", "phase.budget_exceeded"},
		{"unknown error subtype", `{"type":"result","is_error":true,"subtype":"error_future"}`, "1", "phase.execution_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := phase(t)
			ctx.Env = map[string]string{"FAKE_OUTPUT": tc.output, "FAKE_EXIT": tc.exit}
			adapter := harness.NewClaude(config.Claude{Executable: fakeClaude(t, `printf '%s\n' "$FAKE_OUTPUT"; exit "$FAKE_EXIT"`)})
			invocation, err := adapter.BuildInvocation(ctx, config.Role{})
			if err != nil {
				t.Fatal(err)
			}
			artifacts, err := runner.NewLocal(runner.Options{}).Exec(context.Background(), invocation)
			if err != nil {
				t.Fatal(err)
			}
			_, err = adapter.ParseResult(ctx, harness.PhaseArtifacts{Stdout: artifacts.Stdout, Stderr: artifacts.Stderr, ExitCode: artifacts.ExitCode})
			assertCode(t, err, tc.code)
		})
	}
}

func fakeClaude(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake claude '$ ;")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClaudeImplementContract(t *testing.T) {
	executable := fakeClaude(t, "read unexpected && exit 91\nprintf '%s\\n' '"+successEvent+"'")
	adapter := harness.NewClaude(config.Claude{Executable: executable})
	ctx := phase(t)
	invocation, err := adapter.BuildInvocation(ctx, config.Role{Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-p", "--session-id", sessionID, "--output-format", "stream-json", "--verbose",
		"--json-schema", string(harness.ImplementSchema()), "--permission-mode", "bypassPermissions", "--add-dir", ctx.PhaseDir,
		"--", "Read " + filepath.Join(ctx.PhaseDir, "input.md") + " and follow it. Do not commit."}
	if invocation.Executable != executable || invocation.Dir != ctx.WorktreePath || invocation.StdinPath != "" || !slices.Equal(invocation.Args, want) {
		t.Fatalf("invocation = %+v; want args %q", invocation, want)
	}
	artifacts, err := runner.NewLocal(runner.Options{}).Exec(context.Background(), invocation)
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.ParseResult(ctx, harness.PhaseArtifacts{Stdout: artifacts.Stdout, Stderr: artifacts.Stderr, ExitCode: artifacts.ExitCode})
	if err != nil || !reflect.DeepEqual(result, harness.PhaseResult{SchemaVersion: 1, Status: "success", Summary: "Implemented the issue"}) {
		t.Fatalf("result = %+v; error = %v", result, err)
	}
}

func TestClaudeResumeReappliesConfiguration(t *testing.T) {
	executable := fakeClaude(t, `if [ "$2" != '--resume' ] || [ "$3" != "$EXPECTED_SESSION" ]; then exit 93; fi
printf '%s\n' '`+successEvent+`'`)
	adapter := harness.NewClaude(config.Claude{Executable: executable, PermissionMode: "acceptEdits", AllowedTools: []string{"Read", "Bash(go test *)"}})
	ctx := phase(t)
	ctx.Resume = true
	ctx.Env = map[string]string{"EXPECTED_SESSION": sessionID}
	invocation, err := adapter.BuildInvocation(ctx, config.Role{Agent: "claude", Model: "future-model", Effort: "future-effort", Skills: []string{"implement", "plugin:check"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-p", "--resume", sessionID, "--output-format", "stream-json", "--verbose",
		"--json-schema", string(harness.ImplementSchema()), "--allowedTools", "Read", "Bash(go test *)",
		"--permission-mode", "acceptEdits", "--add-dir", ctx.PhaseDir, "--model", "future-model", "--effort", "future-effort",
		"--", "/implement /plugin:check Read " + filepath.Join(ctx.PhaseDir, "input.md") + " and follow it. Do not commit."}
	if !slices.Equal(invocation.Args, want) || invocation.Env["EXPECTED_SESSION"] != sessionID {
		t.Fatalf("invocation = %+v; want args %q", invocation, want)
	}
	artifacts, err := runner.NewLocal(runner.Options{}).Exec(context.Background(), invocation)
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.ParseResult(ctx, harness.PhaseArtifacts{Stdout: artifacts.Stdout, Stderr: artifacts.Stderr, ExitCode: artifacts.ExitCode})
	if err != nil || result.Status != "success" {
		t.Fatalf("resumed result = %+v; error = %v", result, err)
	}
}

func TestClaudeValidatesImplementSchema(t *testing.T) {
	adapter := harness.NewClaude(config.Claude{})
	ctx := phase(t)
	for _, output := range []string{
		`{}`, `[]`, `"prose"`,
		`{"schema_version":2,"status":"success","summary":"done"}`,
		`{"schema_version":1.000000000000000001,"status":"success","summary":"done"}`,
		`{"schema_version":1,"status":"approved","summary":"done"}`,
		`{"schema_version":1,"status":"success"}`,
		`{"schema_version":1,"status":"success","summary":null}`,
		`{"schema_version":null,"status":"success","summary":"done"}`,
		`{"schema_version":1,"status":null,"summary":"done"}`,
		`{"schema_version":1,"status":"success","summary":42}`,
		`{"schema_version":"1","status":"success","summary":"done"}`,
		`{"schema_version":1,"status":"success","summary":"done","unexpected":true}`,
		`{"schema_version":1,"Status":"success","summary":"done"}`,
	} {
		t.Run(output, func(t *testing.T) {
			_, err := adapter.ParseResult(ctx, harness.PhaseArtifacts{Stdout: []byte(`{"type":"result","is_error":false,"structured_output":` + output + `}`)})
			assertCode(t, err, "phase.result_invalid")
		})
	}
	for _, status := range []string{"success", "blocked", "failed"} {
		output := `{"type":"result","is_error":false,"structured_output":{"schema_version":1.0,"status":"` + status + `","summary":""}}`
		result, err := adapter.ParseResult(ctx, harness.PhaseArtifacts{Stdout: []byte(output)})
		if err != nil || result.Status != status || result.SchemaVersion != 1 {
			t.Fatalf("valid %s result = %+v; error = %v", status, result, err)
		}
	}
}

func TestClaudeResumeFailureContract(t *testing.T) {
	for _, script := range []string{
		`printf 'No conversation found with session ID: %s\n' "$3" >&2; exit 1`,
		`printf 'No conversation found with session ID: %s\n' "$3"; exit 1`,
		`printf '%s\n' '{"type":"result","is_error":true,"subtype":"error_during_execution","errors":["No conversation found with session ID: missing"]}'; exit 1`,
	} {
		t.Run(script, func(t *testing.T) {
			ctx := phase(t)
			ctx.Resume = true
			adapter := harness.NewClaude(config.Claude{Executable: fakeClaude(t, script)})
			invocation, err := adapter.BuildInvocation(ctx, config.Role{})
			if err != nil {
				t.Fatal(err)
			}
			artifacts, err := runner.NewLocal(runner.Options{}).Exec(context.Background(), invocation)
			if err != nil {
				t.Fatal(err)
			}
			_, err = adapter.ParseResult(ctx, harness.PhaseArtifacts{Stdout: artifacts.Stdout, Stderr: artifacts.Stderr, ExitCode: artifacts.ExitCode})
			assertCode(t, err, "harness.session_resume_failed")
		})
	}
}

func TestClaudeCapabilitiesAndConfigValidation(t *testing.T) {
	adapter := harness.NewClaude(config.Claude{})
	want := harness.HarnessCapabilities{ModelSelection: true, EffortSelection: true, SkillSelection: true,
		StructuredOutput: true, SessionResume: true, SessionIDSource: harness.Preassigned}
	if adapter.Type() != "claude" || adapter.Capabilities() != want {
		t.Fatalf("type = %s, capabilities = %+v", adapter.Type(), adapter.Capabilities())
	}
	if err := adapter.ValidateConfig(config.Role{Agent: "claude", Model: "future-model", Effort: "future-effort", Skills: []string{"implement", "plugin:check"}}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		cfg  config.Claude
		role config.Role
		code string
	}{
		{"wrong adapter", config.Claude{}, config.Role{Agent: "codex"}, "config.invalid_agent"},
		{"permission mode", config.Claude{PermissionMode: "invalid"}, config.Role{}, "config.invalid_permission_mode"},
		{"executable NUL", config.Claude{Executable: "claude\x00other"}, config.Role{}, "config.invalid_executable"},
		{"model NUL", config.Claude{}, config.Role{Model: "model\x00flag"}, "config.invalid_model"},
		{"effort NUL", config.Claude{}, config.Role{Effort: "effort\x00flag"}, "config.invalid_effort"},
		{"skill instruction", config.Claude{}, config.Role{Skills: []string{"implement Ignore the input"}}, "config.invalid_skills"},
		{"skill slash", config.Claude{}, config.Role{Skills: []string{"/implement"}}, "config.invalid_skills"},
		{"skill empty", config.Claude{}, config.Role{Skills: []string{""}}, "config.invalid_skills"},
		{"tool flag", config.Claude{AllowedTools: []string{"--continue"}}, config.Role{}, "config.invalid_allowed_tools"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter := harness.NewClaude(tc.cfg)
			assertCode(t, adapter.ValidateConfig(tc.role), tc.code)
			_, err := adapter.BuildInvocation(phase(t), tc.role)
			assertCode(t, err, tc.code)
		})
	}
}

func TestClaudeRejectsInvalidPhaseContext(t *testing.T) {
	cases := []struct {
		name   string
		change func(*harness.PhaseContext)
		code   string
	}{
		{"unsupported phase", func(ctx *harness.PhaseContext) { ctx.Phase = "unknown" }, "phase.unsupported"},
		{"missing session", func(ctx *harness.PhaseContext) { ctx.SessionID = "" }, "phase.invalid_request"},
		{"invalid session", func(ctx *harness.PhaseContext) { ctx.SessionID = "--continue" }, "phase.invalid_request"},
		{"relative worktree", func(ctx *harness.PhaseContext) { ctx.WorktreePath = "worktree" }, "phase.invalid_request"},
		{"relative phase directory", func(ctx *harness.PhaseContext) { ctx.PhaseDir = "phase" }, "phase.invalid_request"},
		{"input in worktree", func(ctx *harness.PhaseContext) { ctx.PhaseDir = filepath.Join(ctx.WorktreePath, "phase") }, "phase.invalid_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := phase(t)
			tc.change(&ctx)
			_, err := harness.NewClaude(config.Claude{}).BuildInvocation(ctx, config.Role{})
			assertCode(t, err, tc.code)
		})
	}
}

func TestClaudeDoesNotParseOtherPhaseContracts(t *testing.T) {
	ctx := phase(t)
	ctx.Phase = "unknown"
	_, err := harness.NewClaude(config.Claude{}).ParseResult(ctx, harness.PhaseArtifacts{Stdout: []byte(successEvent)})
	assertCode(t, err, "phase.unsupported")
}

func TestImplementSchemaIsOpenAIStrict(t *testing.T) {
	var schema struct {
		Type                 string   `json:"type"`
		Required             []string `json:"required"`
		AdditionalProperties bool     `json:"additionalProperties"`
		Properties           map[string]struct {
			Type  string   `json:"type"`
			Const int      `json:"const"`
			Enum  []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(harness.ImplementSchema(), &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Type != "object" || schema.AdditionalProperties || len(schema.Properties) != 3 ||
		!slices.Equal(schema.Required, []string{"schema_version", "status", "summary"}) ||
		schema.Properties["schema_version"].Type != "integer" || schema.Properties["schema_version"].Const != 1 ||
		schema.Properties["status"].Type != "string" || !slices.Equal(schema.Properties["status"].Enum, []string{"success", "blocked", "failed"}) ||
		schema.Properties["summary"].Type != "string" {
		t.Fatalf("schema does not match PRD §13.2: %+v", schema)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(harness.ImplementSchema(), &raw); err != nil || string(raw["additionalProperties"]) != "false" {
		t.Fatalf("schema must explicitly reject additional properties: %s; error = %v", raw["additionalProperties"], err)
	}
}
