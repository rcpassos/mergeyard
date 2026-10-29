package testkit

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
type CommandRecorder struct {
	runner.Runner
	Mu       sync.Mutex
	Requests []runner.SessionRequest
}

func (r *CommandRecorder) StartSession(ctx context.Context, request runner.SessionRequest) (runner.SessionRef, error) {
	r.Mu.Lock()
	r.Requests = append(r.Requests, request)
	r.Mu.Unlock()
	return r.Runner.StartSession(ctx, request)
}

func (r *CommandRecorder) Ran(phase string, round, attempt int) (runner.SessionRequest, bool) {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	for _, request := range r.Requests {
		if request.Phase == phase && request.Round == round && request.Attempt == attempt {
			return request, true
		}
	}
	return runner.SessionRequest{}, false
}

// permissionFlow runs one implement-review-fix loop through a recording runner.
func PermissionFlow(t *testing.T, agent string, configure func(*config.Config)) (*CommandRecorder, *app.Runtime) {
	t.Helper()
	script := LoopScript(`printf fixed > feature.txt; ` + FixedReport)
	_, runtime, api, _, cfg, r := TmuxFlow(t, script)
	configure(&cfg)
	// The shared Codex fixture asserts the sandbox flags it receives; pass the
	// literal flags the adapter emits for this configuration.
	codexScript := strings.Replace(NativeCodexScript(script),
		`-s workspace-write -c approval_policy="never" -c sandbox_workspace_write.network_access=true`,
		fmt.Sprintf(`-s %s -c approval_policy="never" -c sandbox_workspace_write.network_access=%t`, cfg.Agents.Codex.Sandbox, cfg.Agents.Codex.NetworkAccess), 1)
	executable := filepath.Join(t.TempDir(), "fake-codex")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+codexScript), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Agents.Codex.Executable = executable
	cfg.Repositories[0].Implementer = config.Role{Agent: agent, Model: "implement-model", Effort: "medium", Skills: []string{"implement"}, MaxAttempts: 1}
	cfg.Repositories[0].Reviewer = config.Role{Agent: agent, Model: "review-model", Effort: "low", Skills: []string{"review"}, MaxAttempts: 1}
	recorder := &CommandRecorder{Runner: r}
	s, err := TestScheduler(cfg, SchedulerResources(runtime), scheduler.Dependencies{GitHub: BranchGitHub{api}, Runner: recorder})
	if err != nil {
		t.Fatal(err)
	}
	if run := Finish(t, s, workflow.WaitingForCI, workflow.Review); run.ReviewRound != 2 || run.Fix == nil {
		t.Fatalf("flow did not finish a fix round: %+v", run)
	}
	return recorder, runtime
}

func AssertAttemptPermissions(t *testing.T, phase string, round, attempt int, wantMode string, wantTools []string, mode, toolsJSON string) {
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
func CommandPermissions(t *testing.T, agent string, args []string) (summary, mode string, tools []string) {
	t.Helper()
	if agent == registry.Claude {
		mode = FlagValue(t, args, "--permission-mode")
		tools = VariadicValues(args, "--allowedTools")
		summary = mode + " · allowed tools " + strings.Join(tools, ", ")
		if disallowed := VariadicValues(args, "--disallowedTools"); len(disallowed) > 0 {
			summary += " · disallowed tools " + strings.Join(disallowed, ", ")
		}
		return summary, mode, tools
	}
	sandbox := FlagValue(t, args, "-s")
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

func FlagValue(t *testing.T, args []string, flag string) string {
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
func VariadicValues(args []string, flag string) []string {
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
