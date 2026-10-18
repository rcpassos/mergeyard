package harness_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// TestReportedPermissionsMatchTheBuiltCommand reads the effective permission
// facts back out of the command each adapter built for one phase. Every
// recorded attempt keeps that same report, so these assertions are what make
// the audit record trustworthy.
func TestReportedPermissionsMatchTheBuiltCommand(t *testing.T) {
	cases := []struct {
		name    string
		phase   workflow.Phase
		adapter harness.HarnessAdapter
	}{
		{"claude defaults", workflow.Implement, harness.NewClaude(config.Claude{})},
		{"claude permission mode and tools", workflow.Implement, harness.NewClaude(config.Claude{PermissionMode: "acceptEdits", AllowedTools: []string{"Read", "Bash(go test *)"}})},
		{"claude review removes the editing tools", workflow.Review, harness.NewClaude(config.Claude{PermissionMode: "acceptEdits", AllowedTools: []string{"Read", "Bash(go test *)"}})},
		{"codex defaults", workflow.Implement, harness.NewCodex(config.Codex{})},
		{"codex network off", workflow.Implement, harness.NewCodex(config.Codex{NetworkAccess: false})},
		{"codex full access ignores the network toggle", workflow.Implement, harness.NewCodex(config.Codex{Sandbox: "danger-full-access", NetworkAccess: false})},
		{"codex review keeps its sandbox", workflow.Review, harness.NewCodex(config.Codex{})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := phase(t)
			ctx.Phase = tc.phase
			built, err := tc.adapter.BuildInvocation(ctx, config.Role{})
			if err != nil {
				t.Fatal(err)
			}
			reported := built.Permissions
			if reported.Summary == "" || reported.Mode == "" {
				t.Fatalf("%s reported no effective permissions: %+v", tc.adapter.Type(), reported)
			}
			switch tc.adapter.Type() {
			case "claude":
				mode := commandFlag(t, built.Command.Args, "--permission-mode")
				tools := variadicFlag(t, built.Command.Args, "--allowedTools")
				disallowed := variadicFlag(t, built.Command.Args, "--disallowedTools")
				if reported.Mode != mode || !slices.Equal(reported.AllowedTools, tools) {
					t.Fatalf("recorded %q/%v does not match the command's %q/%v", reported.Mode, reported.AllowedTools, mode, tools)
				}
				if !strings.Contains(reported.Summary, mode) || !strings.Contains(reported.Summary, strings.Join(tools, ", ")) {
					t.Fatalf("summary %q does not describe the command's %q/%v", reported.Summary, mode, tools)
				}
				if tc.phase == workflow.Review && len(disallowed) == 0 {
					t.Fatal("review must remove the editing tools")
				}
				if len(disallowed) > 0 && !strings.Contains(reported.Summary, "disallowed tools "+strings.Join(disallowed, ", ")) {
					t.Fatalf("summary %q does not record the command's disallowed tools %v", reported.Summary, disallowed)
				}
			case "codex":
				sandbox := commandFlag(t, built.Command.Args, "-s")
				network := codexCommandNetwork(t, built.Command.Args)
				if !strings.Contains(reported.Summary, sandbox) || !strings.Contains(reported.Summary, "network "+network) {
					t.Fatalf("summary %q does not describe the command's %s/network %s", reported.Summary, sandbox, network)
				}
				if reported.Mode != reported.Summary || reported.AllowedTools == nil || len(reported.AllowedTools) != 0 {
					t.Fatalf("codex mode/tools %q/%v do not describe its sandbox surface", reported.Mode, reported.AllowedTools)
				}
			}
		})
	}
}

// commandFlag returns the value immediately after one flag.
func commandFlag(t *testing.T, args []string, flag string) string {
	t.Helper()
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("command has no %s: %q", flag, args)
	return ""
}

// variadicFlag returns the values after a variadic flag, up to the next flag.
// An absent flag means the command granted no such values.
func variadicFlag(t *testing.T, args []string, flag string) []string {
	t.Helper()
	for i, arg := range args {
		if arg != flag {
			continue
		}
		var values []string
		for _, value := range args[i+1:] {
			if strings.HasPrefix(value, "-") {
				break
			}
			values = append(values, value)
		}
		return values
	}
	return nil
}

// codexCommandNetwork reads Codex's effective network access from the command:
// the sandbox it applies and the network override it passes. The workspace-write
// toggle cannot restrict full access, so full access is always effective.
func codexCommandNetwork(t *testing.T, args []string) string {
	t.Helper()
	sandbox := commandFlag(t, args, "-s")
	const key = "sandbox_workspace_write.network_access="
	for i, arg := range args {
		if arg == "-c" && i+1 < len(args) && strings.HasPrefix(args[i+1], key) {
			if sandbox == "danger-full-access" {
				return "true"
			}
			return strings.TrimPrefix(args[i+1], key)
		}
	}
	t.Fatalf("command has no network access override: %q", args)
	return ""
}
