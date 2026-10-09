package harness

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// Availability requests have no phase input, skills, repository, or resumed
// conversation. Their native completion is checked independently of task schemas.
const availabilityPrompt = "Reply only OK. This is an account availability check. Do not use tools, read files, run commands, or perform engineering work."

func (c *Claude) BuildCheckInvocation(dir string, env map[string]string) (Invocation, error) {
	args := []string{"-p", "--output-format", "stream-json", "--verbose", "--tools", "", "--max-turns", "1", "--no-session-persistence", "--disable-slash-commands", "--setting-sources", "", "--settings", `{"disableAllHooks":true}`, "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--", availabilityPrompt}
	return Invocation{Executable: c.config.Executable, Dir: dir, Env: env, Args: args}, nil
}

func (c *Codex) BuildCheckInvocation(dir string, env map[string]string) (Invocation, error) {
	args := []string{"exec", "--json", "--ephemeral", "--skip-git-repo-check", "-C", dir, "-s", "read-only", "-c", `approval_policy="never"`, "-c", `web_search="disabled"`, "-c", "features.shell_tool=false", "-c", "features.code_mode_host=false", "-c", "features.code_mode=false", "-c", "features.multi_agent=false", "-c", "features.hooks=false", "-c", "features.apps=false", "-c", "features.plugins=false", "-c", "project_doc_max_bytes=0", "-c", "features.skip_host_skill_discovery=true", "-c", "skills.bundled.enabled=false", "-c", "skills.include_instructions=false", "-c", "cloud.skills.enabled=false", "-c", `developer_instructions=""`, "--", availabilityPrompt}
	return Invocation{Executable: c.config.Executable, Dir: dir, Env: env, Args: args}, nil
}

func (*Codex) CheckConfigurationInvocation(command Invocation) Invocation {
	inspection := command
	inspection.Args = []string{"-C", command.Dir}
	for i := 0; i < len(command.Args); i++ {
		if command.Args[i] == "-c" && i+1 < len(command.Args) {
			i++
			inspection.Args = append(inspection.Args, "-c", command.Args[i])
		}
	}
	inspection.Args = append(inspection.Args, "mcp", "list", "--json")
	return inspection
}

func (*Codex) ConfigureCheckInvocation(command Invocation, data []byte) (Invocation, error) {
	var servers []struct {
		Name    string `json:"name"`
		Enabled *bool  `json:"enabled"`
	}
	if len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '[' || json.Unmarshal(data, &servers) != nil {
		return Invocation{}, phaseError("harness.check_configuration_failed", "Cannot inspect effective MCP configuration; fix Codex configuration and check again", nil)
	}
	names := map[string]bool{}
	for _, server := range servers {
		if server.Name == "" || server.Enabled == nil {
			return Invocation{}, phaseError("harness.check_configuration_failed", "Codex MCP configuration inspection is incomplete; update Codex and check again", nil)
		}
		names[server.Name] = true
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	entries := make([]string, 0, len(sorted))
	for _, name := range sorted {
		// Quoted keys belong inside the inline TOML table: Codex dotted CLI paths
		// split names on '.', so per-name dotted overrides cannot address every key.
		quoted, _ := json.Marshal(name)
		entries = append(entries, string(quoted)+"={enabled=false}")
	}
	override := "mcp_servers={" + strings.Join(entries, ",") + "}"
	split := len(command.Args) - 2
	if split < 0 || command.Args[split] != "--" {
		return Invocation{}, phaseError("harness.check_configuration_failed", "Availability invocation is invalid; update Mergeyard and check again", nil)
	}
	args := append([]string(nil), command.Args[:split]...)
	args = append(args, "-c", override)
	args = append(args, command.Args[split:]...)
	command.Args = args
	return command, nil
}
