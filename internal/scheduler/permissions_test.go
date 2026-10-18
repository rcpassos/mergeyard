package scheduler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/harness/registry"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// commandRecorder captures every launched request so a test can compare an
// attempt's recorded permissions against the command that actually ran.
type commandRecorder struct {
	runner.Runner
	mu       sync.Mutex
	requests []runner.SessionRequest
}

func (r *commandRecorder) StartSession(ctx context.Context, request runner.SessionRequest) (runner.SessionRef, error) {
	r.mu.Lock()
	r.requests = append(r.requests, request)
	r.mu.Unlock()
	return r.Runner.StartSession(ctx, request)
}

func (r *commandRecorder) ran(phase string, round, attempt int) (runner.SessionRequest, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, request := range r.requests {
		if request.Phase == phase && request.Round == round && request.Attempt == attempt {
			return request, true
		}
	}
	return runner.SessionRequest{}, false
}

// permissionFlow runs one implement-review-fix loop through a recording runner.
func permissionFlow(t *testing.T, agent string, configure func(*config.Config)) (*commandRecorder, *app.Runtime) {
	t.Helper()
	script := loopScript(`printf fixed > feature.txt; ` + fixedReport)
	_, runtime, api, _, cfg, r := localFlow(t, script)
	configure(&cfg)
	// The shared Codex fixture asserts the sandbox flags it receives; pass the
	// literal flags the adapter emits for this configuration.
	codexScript := strings.Replace(nativeCodexScript(script),
		`-s workspace-write -c approval_policy="never" -c sandbox_workspace_write.network_access=true`,
		fmt.Sprintf(`-s %s -c approval_policy="never" -c sandbox_workspace_write.network_access=%t`, cfg.Agents.Codex.Sandbox, cfg.Agents.Codex.NetworkAccess), 1)
	executable := filepath.Join(t.TempDir(), "fake-codex")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+codexScript), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Agents.Codex.Executable = executable
	cfg.Repositories[0].Implementer = config.Role{Agent: agent, Model: "implement-model", Effort: "medium", Skills: []string{"implement"}, MaxAttempts: 1}
	cfg.Repositories[0].Reviewer = config.Role{Agent: agent, Model: "review-model", Effort: "low", Skills: []string{"review"}, MaxAttempts: 1}
	recorder := &commandRecorder{Runner: r}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: recorder})
	if err != nil {
		t.Fatal(err)
	}
	if run := finish(t, s, workflow.WaitingForCI, workflow.Review); run.ReviewRound != 2 || run.Fix == nil {
		t.Fatalf("flow did not finish a fix round: %+v", run)
	}
	return recorder, runtime
}

// TestRecordedPermissionsMatchTheCommandThatRan reads every attempt's audit
// record back out of the command the scheduler actually launched, including the
// editing tools a Claude review removes. This is the persisted chain: adapter
// report, launch journal, phase and review tables.
func TestRecordedPermissionsMatchTheCommandThatRan(t *testing.T) {
	cases := []struct {
		name      string
		agent     string
		configure func(*config.Config)
	}{
		{"claude permission mode and tools", registry.Claude, func(cfg *config.Config) {
			cfg.Agents.Claude.PermissionMode = "acceptEdits"
			cfg.Agents.Claude.AllowedTools = []string{"Read", "Bash(go test *)"}
		}},
		{"codex sandbox and network", registry.Codex, func(cfg *config.Config) {
			cfg.Agents.Codex.Sandbox, cfg.Agents.Codex.NetworkAccess = "workspace-write", true
		}},
		{"codex full access ignores the network toggle", registry.Codex, func(cfg *config.Config) {
			cfg.Agents.Codex.Sandbox, cfg.Agents.Codex.NetworkAccess = "danger-full-access", false
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder, runtime := permissionFlow(t, tc.agent, tc.configure)
			rows, err := runtime.DB.QueryContext(context.Background(), `SELECT a.phase,a.round,a.attempt,COALESCE(a.permissions,''),COALESCE(r.permission_mode,''),COALESCE(r.allowed_tools_json,''),COALESCE(f.permission_mode,''),COALESCE(f.allowed_tools_json,'')
 FROM phase_attempts a
 LEFT JOIN review_attempts r ON r.attempt_id=a.id
 LEFT JOIN fix_attempts f ON f.attempt_id=a.id
 WHERE a.step='' ORDER BY a.phase,a.round,a.attempt`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			attempts := 0
			for rows.Next() {
				var phase, summary, reviewMode, reviewTools, fixMode, fixTools string
				var round, attempt int
				if err := rows.Scan(&phase, &round, &attempt, &summary, &reviewMode, &reviewTools, &fixMode, &fixTools); err != nil {
					t.Fatal(err)
				}
				request, ok := recorder.ran(phase, round, attempt)
				if !ok {
					t.Fatalf("%s attempt %d/%d has no launched command", phase, round, attempt)
				}
				wantSummary, wantMode, wantTools := commandPermissions(t, tc.agent, request.Command.Args)
				if summary != wantSummary {
					t.Fatalf("%s attempt %d/%d recorded permissions %q; the command recorded %q", phase, round, attempt, summary, wantSummary)
				}
				switch phase {
				case "review":
					assertAttemptPermissions(t, phase, round, attempt, wantMode, wantTools, reviewMode, reviewTools)
				case "fix":
					assertAttemptPermissions(t, phase, round, attempt, wantMode, wantTools, fixMode, fixTools)
				}
				attempts++
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			// One implement, two reviews and one fix ran in this flow.
			if attempts != 4 {
				t.Fatalf("checked %d attempts, want 4", attempts)
			}
		})
	}
}

func assertAttemptPermissions(t *testing.T, phase string, round, attempt int, wantMode string, wantTools []string, mode, toolsJSON string) {
	t.Helper()
	wantToolsJSON, err := json.Marshal(wantTools)
	if err != nil {
		t.Fatal(err)
	}
	var tools []string
	if err := json.Unmarshal([]byte(toolsJSON), &tools); err != nil {
		t.Fatal(err)
	}
	if mode != wantMode || toolsJSON != string(wantToolsJSON) || !slices.Equal(tools, wantTools) {
		t.Fatalf("%s attempt %d/%d recorded %q/%s; the command recorded %q/%s", phase, round, attempt, mode, toolsJSON, wantMode, wantToolsJSON)
	}
}

// commandPermissions reads the expected audit record out of the command line.
func commandPermissions(t *testing.T, agent string, args []string) (summary, mode string, tools []string) {
	t.Helper()
	if agent == registry.Claude {
		mode = flagValue(t, args, "--permission-mode")
		tools = variadicValues(args, "--allowedTools")
		summary = mode + " · allowed tools " + strings.Join(tools, ", ")
		if disallowed := variadicValues(args, "--disallowedTools"); len(disallowed) > 0 {
			summary += " · disallowed tools " + strings.Join(disallowed, ", ")
		}
		return summary, mode, tools
	}
	sandbox := flagValue(t, args, "-s")
	network := "false"
	const key = "sandbox_workspace_write.network_access="
	for i, arg := range args {
		if arg == "-c" && i+1 < len(args) && strings.HasPrefix(args[i+1], key) {
			network = strings.TrimPrefix(args[i+1], key)
		}
	}
	if sandbox == "danger-full-access" {
		// The workspace-write toggle cannot restrict full access.
		network = "true"
	}
	summary = sandbox + " · network " + network + " · approvals never"
	return summary, summary, []string{}
}

func flagValue(t *testing.T, args []string, flag string) string {
	t.Helper()
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("command has no %s: %q", flag, args)
	return ""
}

// variadicValues returns the values after a variadic flag, up to the next flag.
// An absent flag records the empty list, not an absent one.
func variadicValues(args []string, flag string) []string {
	values := []string{}
	for i, arg := range args {
		if arg != flag {
			continue
		}
		for _, value := range args[i+1:] {
			if strings.HasPrefix(value, "-") {
				break
			}
			values = append(values, value)
		}
		return values
	}
	return values
}
