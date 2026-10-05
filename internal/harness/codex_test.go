package harness_test

import (
	"context"
	"github.com/rcpassos/mergeyard/internal/runner"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/harness"
)

func TestCodexInvocation(t *testing.T) {
	ctx := phase(t)
	ctx.SessionID = ""
	if _, err := harness.WriteImplementInput(context.Background(), runner.NewLocal(runner.Options{}), ctx, harness.ImplementInput{IssueNumber: 40, IssueBody: "complete context"}); err != nil {
		t.Fatal(err)
	}
	adapter := harness.NewCodex(config.Codex{Sandbox: "workspace-write", NetworkAccess: true})
	invocation, err := adapter.BuildInvocation(ctx, config.Role{Agent: "codex", Model: "chosen-model", Effort: "medium", Skills: []string{"implement", "check"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "--json", "-C", ctx.WorktreePath, "-s", "workspace-write", "-c", "approval_policy=\"never\"", "-c", "sandbox_workspace_write.network_access=true", "--output-schema", filepath.Join(ctx.PhaseDir, "schema.json"), "-o", filepath.Join(ctx.PhaseDir, "last-message.json"), "--model", "chosen-model", "-c", "model_reasoning_effort=\"medium\"", "--", "$implement $check Read " + filepath.Join(ctx.PhaseDir, "input.md") + " and follow it. Do not commit."}
	if !slices.Equal(invocation.Args, want) {
		t.Fatalf("invocation: %q", invocation.Args)
	}
	if invocation.Dir != ctx.WorktreePath || invocation.Executable != "codex" {
		t.Fatalf("command: %+v", invocation)
	}
	schema, err := os.ReadFile(filepath.Join(ctx.PhaseDir, "schema.json"))
	if err != nil || string(schema) != string(harness.ImplementSchema()) {
		t.Fatalf("schema %s: %v", schema, err)
	}
	ctx.SessionID, ctx.Resume = sessionID, true
	resumed, err := adapter.BuildInvocation(ctx, config.Role{Agent: "codex"})
	if err != nil || !slices.Equal(resumed.Args[len(resumed.Args)-4:len(resumed.Args)-1], []string{"resume", "--", sessionID}) {
		t.Fatalf("resume: %q, %v", resumed.Args, err)
	}
	if strings.Contains(strings.Join(resumed.Args, " "), "--last") {
		t.Fatal("resume must use exact ID")
	}
}

const codexSuccess = `{"type":"thread.started","thread_id":"7f9e8221-470e-43e0-adc7-41798c5c193e"}
{"type":"item.completed","item":{"type":"agent_message","text":"{\"schema_version\":1,\"status\":\"success\",\"summary\":\"Added feature\"}"}}
{"type":"turn.completed"}
`

func TestCodexRequiresNativeCompletionAndStrictResult(t *testing.T) {
	adapter := harness.NewCodex(config.Codex{})
	ctx := phase(t)
	ctx.SessionID = ""
	result, err := adapter.ParseResult(ctx, harness.PhaseArtifacts{Stdout: []byte(codexSuccess), LastMessage: []byte(`{"schema_version":1,"status":"success","summary":"Added feature"}`)})
	if err != nil || result.Status != "success" || result.Summary != "Added feature" {
		t.Fatalf("result %+v: %v", result, err)
	}
	for _, tc := range []struct {
		name, output, stderr, code string
		exit                       int
	}{
		{"incomplete", strings.ReplaceAll(codexSuccess, `{"type":"turn.completed"}`, ""), "", "phase.result_missing", 0},
		{"interrupted", codexSuccess, "Terminated", "phase.execution_failed", 143},
		{"invalid", strings.ReplaceAll(codexSuccess, `\"schema_version\":1,`, ""), "", "phase.result_invalid", 0},
		{"failed turn", codexSuccess + "{\"type\":\"turn.failed\",\"error\":{\"message\":\"Unsupported model\"}}\n", "", "phase.execution_failed", 0},
		{"truncated", codexSuccess + `{"type":`, "", "phase.result_invalid", 0},
		{"startup", "", "invalid configuration", "phase.execution_failed", 1},
		{"missing-message", codexSuccess, "", "phase.result_missing", 0},
		{"provider", `{"type":"error","message":"Unsupported model"}` + "\n", "", "phase.execution_failed", 1},
		{"missing session", "", "Error: thread/resume: thread/resume failed: no rollout found for thread id " + sessionID + " (code -32600)", "harness.session_resume_failed", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := []byte(`{"schema_version":1,"status":"success","summary":"Added feature"}`)
			if tc.name == "missing-message" {
				message = nil
			}
			if tc.name == "invalid" {
				message = []byte(`{"status":"success","summary":"Added feature"}`)
			}
			ctx.Resume, ctx.SessionID = tc.name == "missing session", sessionID
			_, err := adapter.ParseResult(ctx, harness.PhaseArtifacts{LastMessage: message, Stdout: []byte(tc.output), Stderr: []byte(tc.stderr), ExitCode: tc.exit})
			assertCode(t, err, tc.code)
			if tc.name == "provider" && !strings.Contains(err.Error(), "Unsupported model") {
				t.Fatalf("lost provider diagnostic: %v", err)
			}
			if tc.stderr != "" && !strings.Contains(err.Error(), tc.stderr) {
				t.Fatalf("lost diagnostic: %v", err)
			}
		})
	}
}

func TestCodexDiscoversIdentityFromLivePrefix(t *testing.T) {
	adapter := harness.NewCodex(config.Codex{})
	id, err := adapter.DiscoverSession([]byte("{\"type\":\"thread.started\",\"thread_id\":\"" + sessionID + "\"}\n{\"type\":"))
	if err != nil || id != sessionID {
		t.Fatalf("early identity %q: %v", id, err)
	}
	id, err = adapter.DiscoverSession([]byte(`{"type":"thread.started","thread_id":"` + sessionID))
	if err != nil || id != "" {
		t.Fatalf("partial event %q: %v", id, err)
	}
	_, err = adapter.DiscoverSession([]byte("{\"type\":\"thread.started\",\"thread_id\":\"bad-id\"}\n"))
	assertCode(t, err, "harness.session_identity_invalid")
}

func TestCodexRetainsIdentityBeforeLaterMalformedOutput(t *testing.T) {
	id, err := harness.NewCodex(config.Codex{}).DiscoverSession([]byte("{\"type\":\"thread.started\",\"thread_id\":\"" + sessionID + "\"}\nmalformed\n"))
	if err != nil || id != sessionID {
		t.Fatalf("lost early identity %q: %v", id, err)
	}
}
