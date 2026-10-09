package harness

// Availability requests have no phase input, skills, repository, or resumed
// conversation. Their native completion is checked independently of task schemas.
const availabilityPrompt = "Reply only OK. This is an account availability check. Do not use tools, read files, run commands, or perform engineering work."

func (c *Claude) BuildCheckInvocation(dir string, env map[string]string) (Invocation, error) {
	args := []string{"-p", "--output-format", "stream-json", "--verbose", "--tools", "", "--max-turns", "1", "--no-session-persistence", "--disable-slash-commands", "--setting-sources", "", "--settings", `{"disableAllHooks":true}`, "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--", availabilityPrompt}
	return Invocation{Executable: c.config.Executable, Dir: dir, Env: env, Args: args}, nil
}

func (c *Codex) BuildCheckInvocation(dir string, env map[string]string) (Invocation, error) {
	args := []string{"exec", "--json", "--ephemeral", "--skip-git-repo-check", "-C", dir, "-s", "read-only", "-c", `approval_policy="never"`, "-c", `web_search="disabled"`, "-c", "features.shell_tool=false", "-c", "features.code_mode_host=false", "-c", "features.code_mode=false", "-c", "features.multi_agent=false", "-c", "features.hooks=false", "-c", "features.apps=false", "-c", "features.plugins=false", "-c", "project_doc_max_bytes=0", "-c", "features.skip_host_skill_discovery=true", "-c", "skills.bundled.enabled=false", "-c", "skills.include_instructions=false", "-c", "cloud.skills.enabled=false", "-c", `developer_instructions=""`, "-c", "mcp_servers={}", "--", availabilityPrompt}
	return Invocation{Executable: c.config.Executable, Dir: dir, Env: env, Args: args}, nil
}
