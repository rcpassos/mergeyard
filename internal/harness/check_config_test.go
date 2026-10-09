package harness_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
)

// Opt-in native configuration check: local config inspection only, with no model
// requests, credentials, server initialization, or inherited user configuration.
func TestCodexCheckDisablesInheritedMCPServers(t *testing.T) {
	if os.Getenv("MERGEYARD_CODEX_CONFIG_INTEGRATION") != "1" {
		t.Skip("opt in to offline native configuration inspection")
	}
	executable, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	dir := t.TempDir()
	cfg := `[mcp_servers.direct]
command = "/no/such/mergeyard-test-server"
required = true
[mcp_servers."name.with.dot"]
command = "/no/such/mergeyard-test-server"
enabled = true
[mcp_servers.disabled]
command = "/no/such/mergeyard-test-server"
enabled = false
`
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"PATH": os.Getenv("PATH"), "HOME": os.Getenv("HOME"), "CODEX_HOME": home}
	adapter := harness.NewCodex(config.Codex{Executable: executable})
	command, err := adapter.BuildCheckInvocation(dir, env)
	if err != nil {
		t.Fatal(err)
	}
	inspect := adapter.CheckConfigurationInvocation(command)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	r := runner.NewLocal(runner.Options{})
	// Inspection itself never initializes a configured MCP server.
	result, err := r.Exec(ctx, inspect)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("configuration inspection failed (exit %d): %v", result.ExitCode, err)
	}
	var baseline []struct {
		Enabled bool `json:"enabled"`
	}
	if json.Unmarshal(result.Stdout, &baseline) != nil {
		t.Fatal("baseline configuration inspection did not return JSON")
	}
	enabledBefore := 0
	for _, server := range baseline {
		if server.Enabled {
			enabledBefore++
		}
	}
	if enabledBefore != 2 {
		t.Fatalf("baseline enabled servers: %d", enabledBefore)
	}

	command, err = adapter.ConfigureCheckInvocation(command, result.Stdout)
	if err != nil {
		t.Fatal(err)
	}
	inspect = adapter.CheckConfigurationInvocation(command)
	result, err = r.Exec(ctx, inspect)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("isolated configuration inspection failed (exit %d): %v", result.ExitCode, err)
	}

	var servers []struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal(result.Stdout, &servers); err != nil {
		t.Fatal("configuration inspection did not return JSON")
	}
	if len(servers) != 3 {
		t.Fatalf("configured server count: %d", len(servers))
	}
	enabled := 0
	for _, server := range servers {
		if server.Enabled {
			enabled++
		}
	}
	if enabled != 0 {
		t.Fatalf("check retained %d enabled MCP servers", enabled)
	}
}

func TestCodexCheckConfigurationRejectsIncompleteInspection(t *testing.T) {
	adapter := harness.NewCodex(config.Codex{})
	command, err := adapter.BuildCheckInvocation(t.TempDir(), map[string]string{"OPENAI_API_KEY": "synthetic-test-value"})
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{"", "null", `{"type":"turn.completed"}`, `[{}]`, `[{"name":"required"}]`, `[{"name":"","enabled":true}]`} {
		if _, err := adapter.ConfigureCheckInvocation(command, []byte(data)); err == nil {
			t.Fatalf("accepted incomplete inspection %q", data)
		}
	}
	configured, err := adapter.ConfigureCheckInvocation(command, []byte(`[{"name":"direct","enabled":true},{"name":"name.with.dot","enabled":true},{"name":"disabled","enabled":false}]`))
	if err != nil {
		t.Fatal(err)
	}
	if configured.Executable != command.Executable || configured.Dir != command.Dir || configured.Env["OPENAI_API_KEY"] != "synthetic-test-value" {
		t.Fatal("configuration isolation changed account environment")
	}
	// A local inspection uses mcp list, never a model execution or a resumed session.
	inspection := adapter.CheckConfigurationInvocation(configured)
	if !slices.Equal(inspection.Args[len(inspection.Args)-3:], []string{"mcp", "list", "--json"}) {
		t.Fatal("inspection would invoke a model")
	}
}
