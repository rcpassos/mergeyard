package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
)

func TestMinimalConfigUsesPRDDefaults(t *testing.T) {
	got, _, err := config.Parse([]byte("repositories: [{repo: rcpassos/mergeyard}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	role := config.Role{Agent: "claude", Skills: []string{}, MaxAttempts: 1}
	want := config.Config{
		Version: 1, Port: 7331, OpenBrowser: true, Workspace: "~/.mergeyard",
		PollInterval: 30 * time.Second, CITimeout: time.Hour, Concurrency: 1,
		Labels: config.Labels{Ready: "ready-for-agent", Running: "agent-running", NeedsAttention: "agent-needs-attention"},
		Agents: config.Agents{
			Claude: config.Claude{Executable: "claude", PermissionMode: "bypassPermissions", AllowedTools: []string{}},
			Codex:  config.Codex{Executable: "codex", Sandbox: "workspace-write", NetworkAccess: true},
		},
		Implementer: role, Reviewer: role, MaxRounds: 5, PRComments: true,
		UsageLimits: config.UsageLimits{Cooldown: 30 * time.Minute, MaxWaits: 3},
		Repositories: []config.Repository{{Repo: "rcpassos/mergeyard", Concurrency: 1, Enabled: true, Implementer: role, Reviewer: role,
			Labels: config.Labels{Ready: "ready-for-agent", Running: "agent-running", NeedsAttention: "agent-needs-attention"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("config = %#v; want %#v", got, want)
	}
}

func TestCITimeout(t *testing.T) {
	for _, value := range []string{"0s", "-1m", "broken", "null"} {
		if _, _, err := config.Parse([]byte("ci_timeout: " + value)); err == nil {
			t.Fatalf("accepted ci_timeout %s", value)
		}
	}
	cfg, _, err := config.Parse([]byte("ci_timeout: 15m"))
	if err != nil || cfg.CITimeout != 15*time.Minute {
		t.Fatalf("timeout: %v %v", cfg.CITimeout, err)
	}
}

func TestLoadSearchOrder(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		explicit, local, global string
		want                    int
		code                    string
	}{
		{"explicit wins", "concurrency: 4", "concurrency: 3", "concurrency: 2", 4, ""},
		{"local wins", "", "concurrency: 3", "concurrency: 2", 3, ""},
		{"global fallback", "", "", "concurrency: 2", 2, ""},
		{"missing", "", "", "", 0, "config.not_found"},
		{"explicit missing never falls back", "missing", "concurrency: 3", "concurrency: 2", 0, "config.not_found"},
		{"invalid local never falls back", "", "concurrency: 0", "concurrency: 2", 0, "config.invalid_concurrency"},
		{"local read failure never falls back", "", "directory", "concurrency: 2", 0, "config.read_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, home := t.TempDir(), t.TempDir()
			t.Chdir(dir)
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // the PRD specifies ~/.config
			globalPath := filepath.Join(home, ".config", "mergeyard", "config.yaml")
			if tc.global != "" {
				writeFixture(t, globalPath, tc.global)
			}
			if tc.local == "directory" {
				if err := os.Mkdir("mergeyard.yaml", 0700); err != nil {
					t.Fatal(err)
				}
			} else if tc.local != "" {
				writeFixture(t, "mergeyard.yaml", tc.local)
			}
			path, wantPath := "", "mergeyard.yaml"
			if tc.local == "" {
				wantPath = globalPath
			}
			if tc.explicit != "" {
				path, wantPath = filepath.Join(dir, "explicit.yaml"), filepath.Join(dir, "explicit.yaml")
				if tc.explicit != "missing" {
					writeFixture(t, path, tc.explicit)
				}
			}
			cfg, doc, err := config.Load(path)
			if tc.code != "" {
				var coded *config.Error
				if !errors.As(err, &coded) || coded.Code != tc.code {
					t.Fatalf("error = %v; want %s", err, tc.code)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Concurrency != tc.want || doc.Path != wantPath {
				t.Errorf("concurrency = %d, source = %q; want %d, %q", cfg.Concurrency, doc.Path, tc.want, wantPath)
			}
		})
	}
}

func writeFixture(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestWriterPreservesCommentsAndUnknownFields(t *testing.T) {
	input := `# user's config
implementer:
  agent: claude # agent choice
  future_role_option: keep-me
repositories:
  # primary repo
  - repo: owner/one # repository note
    future_repo_option: {nested: [one, two]}
future_option: &future "preserve quotes" # future comment
future_alias: *future
# end note
`
	_, doc, err := config.Parse([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	for _, edit := range []struct {
		path  []string
		value any
	}{
		{[]string{"implementer", "agent"}, "codex"},
		{[]string{"repositories", "0", "base_branch"}, "develop"},
		{[]string{"repositories", "-"}, map[string]any{"repo": "owner/two"}},
	} {
		if err := doc.Set(edit.path, edit.value); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := doc.Write(path); err != nil {
		t.Fatal(err)
	}
	cfg, loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Implementer.Agent != "codex" || len(cfg.Repositories) != 2 || cfg.Repositories[0].BaseBranch != "develop" || cfg.Repositories[1].Repo != "owner/two" {
		t.Errorf("written config = %#v", cfg)
	}
	if err := loaded.Write(path); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"# user's config", "# agent choice", "future_role_option: keep-me", "# primary repo", "# repository note", "future_repo_option: {nested: [one, two]}", `&future "preserve quotes"`, "# future comment", "future_alias: *future", "# end note"} {
		if !strings.Contains(string(output), content) {
			t.Errorf("writer lost %q:\n%s", content, output)
		}
	}
	if strings.Contains(string(output), "max_rounds:") {
		t.Error("writer materialized an unspecified default")
	}
}

func TestWriterCreatesConfigAndPreservesFileMode(t *testing.T) {
	_, doc, err := config.Parse([]byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Set([]string{"implementer", "agent"}, "codex"); err != nil {
		t.Fatal(err)
	}
	if err := doc.Set([]string{"repositories"}, []map[string]string{{"repo": "owner/repo"}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	if err := doc.Write(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("new file mode = %v", info.Mode())
	}
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	if err := doc.Write(path); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 {
		t.Errorf("existing file mode = %v", info.Mode())
	}
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Repositories[0].Implementer.Agent != "codex" {
		t.Errorf("new config = %#v", cfg)
	}
}

func TestWriterRejectsInvalidChangesWithoutReplacingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFixture(t, path, "concurrency: 1\n")
	_, doc, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Set([]string{"concurrency"}, 0); err != nil {
		t.Fatal(err)
	}
	var coded *config.Error
	if err := doc.Write(path); !errors.As(err, &coded) || coded.Code != "config.invalid_concurrency" {
		t.Fatalf("error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "concurrency: 1\n" {
		t.Errorf("invalid write changed original: %s", got)
	}
}

func TestDocumentRejectsInvalidPaths(t *testing.T) {
	for _, path := range [][]string{nil, {""}, {"implementer", "agent", "child"}, {"repositories", "-1"}, {"repositories", "1"}, {"repositories", "many"}, {"repositories", "-", "repo"}, {"role_alias", "agent"}, {"role_alias"}, {"missing", "", "child"}, {"missing", "nested", "-", "repo"}} {
		t.Run(strings.Join(path, "/"), func(t *testing.T) {
			_, doc, err := config.Parse([]byte("implementer: &role {agent: claude}\nrole_alias: *role\nrepositories: [{repo: owner/repo}]"))
			if err != nil {
				t.Fatal(err)
			}
			var coded *config.Error
			if err := doc.Set(path, "codex"); !errors.As(err, &coded) || coded.Code != "config.invalid_path" {
				t.Errorf("error = %v; want config.invalid_path", err)
			}
			out := filepath.Join(t.TempDir(), "config.yaml")
			if err := doc.Write(out); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "missing:") {
				t.Errorf("failed edit mutated document: %s", data)
			}
		})
	}
}

func TestWriterReturnsIOError(t *testing.T) {
	_, doc, err := config.Parse([]byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "file.yaml")
	writeFixture(t, path, "unchanged")
	var coded *config.Error
	if err := doc.Write(filepath.Join(path, "config.yaml")); !errors.As(err, &coded) || coded.Code != "config.write_failed" {
		t.Errorf("error = %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "unchanged" {
		t.Errorf("original file = %q, error %v", data, err)
	}
}

func TestAnchoredRoleCanBeReadAndEdited(t *testing.T) {
	data := "implementer: &role {agent: codex, skills: [implement]}\nreviewer: *role\nrepositories: [{repo: owner/repo}]"
	cfg, doc, err := config.Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Reviewer.Agent != "codex" || !reflect.DeepEqual(cfg.Reviewer.Skills, []string{"implement"}) {
		t.Errorf("reviewer = %#v", cfg.Reviewer)
	}
	if err := doc.Set([]string{"implementer", "agent"}, "claude"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := doc.Write(path); err != nil {
		t.Fatal(err)
	}
	cfg, _, err = config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Implementer.Agent != "claude" || cfg.Reviewer.Agent != "claude" {
		t.Errorf("edited anchored roles = %#v, %#v", cfg.Implementer, cfg.Reviewer)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	cfg, _, err := config.Load("../../mergeyard.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Repositories) != 2 || cfg.Repositories[1].Reviewer.Agent != "codex" || cfg.Repositories[1].Reviewer.Effort != "high" {
		t.Errorf("example config = %#v", cfg)
	}
	minimal, _, err := config.Parse([]byte("repositories: [{repo: owner/repo}]"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Repositories, minimal.Repositories = nil, nil
	if !reflect.DeepEqual(cfg, minimal) {
		t.Errorf("example globals differ from built-in defaults:\ngot %#v\nwant %#v", cfg, minimal)
	}
}

func TestGlobalFieldsOverrideDefaults(t *testing.T) {
	data := `version: 2
port: 7444
open_browser: false
workspace: /tmp/work
poll_interval: 1m5s
concurrency: 4
labels: {ready: ready, running: running, needs_attention: attention}
agents:
  claude: {executable: /opt/claude, permission_mode: acceptEdits, allowed_tools: ["Bash(go test:*)"]}
  codex: {executable: /opt/codex, sandbox: danger-full-access, network_access: false}
implementer: {agent: codex, model: a-model, effort: medium, skills: [implement], max_attempts: 2}
reviewer: {agent: claude, model: opus, effort: high, skills: [code-review], max_attempts: 3}
max_rounds: 7
pr_comments: false
usage_limits: {cooldown: 2h, max_waits: 4}
repositories: [{repo: owner/repo}]
`
	got, _, err := config.Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 2 || got.Port != 7444 || got.OpenBrowser || got.Workspace != "/tmp/work" || got.PollInterval != 65*time.Second || got.Concurrency != 4 || got.MaxRounds != 7 || got.PRComments || got.UsageLimits.Cooldown != 2*time.Hour || got.UsageLimits.MaxWaits != 4 {
		t.Errorf("global config = %#v", got)
	}
	if !reflect.DeepEqual(got.Agents, config.Agents{Claude: config.Claude{Executable: "/opt/claude", PermissionMode: "acceptEdits", AllowedTools: []string{"Bash(go test:*)"}}, Codex: config.Codex{Executable: "/opt/codex", Sandbox: "danger-full-access", NetworkAccess: false}}) {
		t.Errorf("agents = %#v", got.Agents)
	}
	if !reflect.DeepEqual(got.Implementer, config.Role{Agent: "codex", Model: "a-model", Effort: "medium", Skills: []string{"implement"}, MaxAttempts: 2}) || !reflect.DeepEqual(got.Reviewer, config.Role{Agent: "claude", Model: "opus", Effort: "high", Skills: []string{"code-review"}, MaxAttempts: 3}) {
		t.Errorf("roles = %#v, %#v", got.Implementer, got.Reviewer)
	}
	if got.Repositories[0].Labels != (config.Labels{Ready: "ready", Running: "running", NeedsAttention: "attention"}) {
		t.Errorf("labels = %#v", got.Repositories[0].Labels)
	}
}

func TestValidationErrorsHaveStableCodesAndPaths(t *testing.T) {
	cases := []struct{ name, data, code, path string }{
		{"repo missing", "repositories: [{}]", "config.invalid_repo", "repositories[0].repo"},
		{"repo format", "repositories: [{repo: owner/repo/extra}]", "config.invalid_repo", "repositories[0].repo"},
		{"repo whitespace", "repositories: [{repo: 'owner/re po'}]", "config.invalid_repo", "repositories[0].repo"},
		{"repo dot", "repositories: [{repo: owner/..}]", "config.invalid_repo", "repositories[0].repo"},
		{"duplicate repository", "repositories: [{repo: owner/repo}, {repo: owner/repo}]", "config.duplicate_repo", "repositories[1].repo"},
		{"case variant repository", "repositories: [{repo: owner/repo}, {repo: OWNER/REPO, enabled: false}]", "config.duplicate_repo", "repositories[1].repo"},
		{"empty ready label", "labels: {ready: ''}", "config.invalid_labels", "labels.ready"},
		{"blank running label", "labels: {running: '   '}", "config.invalid_labels", "labels.running"},
		{"identical labels", "labels: {ready: same, running: same}", "config.invalid_labels", "labels.running"},
		{"case variant labels", "labels: {ready: same, needs_attention: SAME}", "config.invalid_labels", "labels.needs_attention"},
		{"empty repository label", "repositories: [{repo: owner/repo, labels: {needs_attention: ''}}]", "config.invalid_labels", "repositories[0].labels.needs_attention"},
		{"inherited label collision", "labels: {ready: custom}\nrepositories: [{repo: owner/repo, labels: {running: custom}}]", "config.invalid_labels", "repositories[0].labels.running"},
		{"implementer agent", "implementer: {agent: other}", "config.invalid_agent", "implementer.agent"},
		{"reviewer agent", "reviewer: {agent: ''}", "config.invalid_agent", "reviewer.agent"},
		{"repository agent", "repositories: [{repo: owner/repo, reviewer: {agent: other}}]", "config.invalid_agent", "repositories[0].reviewer.agent"},
		{"global concurrency", "concurrency: 0", "config.invalid_concurrency", "concurrency"},
		{"repository concurrency", "repositories: [{repo: owner/repo, concurrency: -1}]", "config.invalid_concurrency", "repositories[0].concurrency"},
		{"rounds", "max_rounds: -1", "config.invalid_max_rounds", "max_rounds"},
		{"implementer attempts", "implementer: {max_attempts: 0}", "config.invalid_max_attempts", "implementer.max_attempts"},
		{"reviewer attempts", "reviewer: {max_attempts: -3}", "config.invalid_max_attempts", "reviewer.max_attempts"},
		{"repository attempts", "repositories: [{repo: owner/repo, implementer: {max_attempts: 0}}]", "config.invalid_max_attempts", "repositories[0].implementer.max_attempts"},
		{"waits", "usage_limits: {max_waits: 0}", "config.invalid_max_waits", "usage_limits.max_waits"},
		{"poll duration", "poll_interval: tomorrow", "config.invalid_duration", "poll_interval"},
		{"cooldown duration", "usage_limits: {cooldown: 3days}", "config.invalid_duration", "usage_limits.cooldown"},
		{"numeric duration", "poll_interval: 30", "config.invalid_duration", "poll_interval"},
		{"zero poll interval", "poll_interval: 0s", "config.invalid_duration", "poll_interval"},
		{"negative poll interval", "poll_interval: -5s", "config.invalid_duration", "poll_interval"},
		{"zero cooldown", "usage_limits: {cooldown: 0s}", "config.invalid_duration", "usage_limits.cooldown"},
		{"negative cooldown", "usage_limits: {cooldown: -5m}", "config.invalid_duration", "usage_limits.cooldown"},
		{"permission", "agents: {claude: {permission_mode: prompt}}", "config.invalid_permission_mode", "agents.claude.permission_mode"},
		{"sandbox", "agents: {codex: {sandbox: read-only}}", "config.invalid_sandbox", "agents.codex.sandbox"},
		{"malformed YAML", "repositories: [", "config.invalid_yaml", "document"},
		{"empty YAML", "", "config.invalid_yaml", "document"},
		{"multiple documents", "{}\n---\n{}", "config.invalid_yaml", "document"},
		{"root sequence", "[]", "config.invalid_yaml", ""},
		{"duplicate field", "concurrency: 1\nconcurrency: 2", "config.invalid_yaml", "concurrency"},
		{"bad bool", "open_browser: maybe", "config.invalid_yaml", "open_browser"},
		{"bad section", "implementer: []", "config.invalid_yaml", "implementer"},
		{"bad repos", "repositories: {}", "config.invalid_yaml", "repositories"},
		{"bad repository", "repositories: [null]", "config.invalid_yaml", "repositories[0]"},
		{"null bool", "open_browser: null", "config.invalid_yaml", "open_browser"},
	}
	for _, key := range []string{"concurrency", "max_rounds", "implementer.max_attempts", "usage_limits.max_waits"} {
		parts := strings.Split(key, ".")
		for _, value := range []string{"0", "-1", ".inf", "-.inf", ".nan", "1.5", "99999999999999999999999999999", "null", "many"} {
			data := parts[len(parts)-1] + ": " + value
			if len(parts) == 2 {
				data = parts[0] + ": {" + data + "}"
			}
			cases = append(cases, struct{ name, data, code, path string }{key + "/" + value, data, "config.invalid_" + parts[len(parts)-1], key})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := config.Parse([]byte(tc.data))
			var coded *config.Error
			if !errors.As(err, &coded) {
				t.Fatalf("error = %v; want *config.Error", err)
			}
			if coded.Code != tc.code || coded.Path != tc.path {
				t.Errorf("error = %v; want %s at %q", err, tc.code, tc.path)
			}
		})
	}
}

func TestValidAgentSettingsPassThrough(t *testing.T) {
	for _, permission := range []string{"auto", "acceptEdits", "bypassPermissions"} {
		for _, sandbox := range []string{"workspace-write", "danger-full-access"} {
			data := "agents: {claude: {permission_mode: " + permission + "}, codex: {sandbox: " + sandbox + "}}\nimplementer: {agent: codex, model: future-model, effort: future-effort}\n"
			cfg, _, err := config.Parse([]byte(data))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Implementer.Model != "future-model" || cfg.Implementer.Effort != "future-effort" || cfg.Agents.Claude.PermissionMode != permission || cfg.Agents.Codex.Sandbox != sandbox {
				t.Errorf("settings = %#v", cfg)
			}
		}
	}
}

func TestRepositoryOverridesOnlySuppliedFields(t *testing.T) {
	cases := []struct {
		name, override string
		want           config.Role
	}{
		{"inherit", "", config.Role{Agent: "codex", Model: "custom-model", Effort: "custom-effort", Skills: []string{"implement"}, MaxAttempts: 4}},
		{"partial", "implementer: {effort: high}", config.Role{Agent: "codex", Model: "custom-model", Effort: "high", Skills: []string{"implement"}, MaxAttempts: 4}},
		{"clear", "implementer: {model: null, effort: null, skills: []}", config.Role{Agent: "codex", Skills: []string{}, MaxAttempts: 4}},
		{"replace", "implementer: {agent: claude, skills: [fix], max_attempts: 2}", config.Role{Agent: "claude", Model: "custom-model", Effort: "custom-effort", Skills: []string{"fix"}, MaxAttempts: 2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := "concurrency: 3\nlabels: {ready: custom-ready}\nimplementer: {agent: codex, model: custom-model, effort: custom-effort, skills: [implement], max_attempts: 4}\nreviewer: {agent: codex, model: review-model}\nrepositories:\n  - repo: owner/one\n    " + tc.override + "\n    reviewer: {effort: high}\n    base_branch: develop\n    concurrency: 2\n    enabled: false\n    labels: {running: custom-running}\n  - repo: owner/two\n"
			cfg, _, err := config.Parse([]byte(data))
			if err != nil {
				t.Fatal(err)
			}
			first, second := cfg.Repositories[0], cfg.Repositories[1]
			if !reflect.DeepEqual(first.Implementer, tc.want) {
				t.Errorf("implementer = %#v; want %#v", first.Implementer, tc.want)
			}
			if first.Reviewer.Agent != "codex" || first.Reviewer.Model != "review-model" || first.Reviewer.Effort != "high" {
				t.Errorf("reviewer = %#v", first.Reviewer)
			}
			if first.Concurrency != 2 || first.Enabled || first.BaseBranch != "develop" || first.Labels.Ready != "custom-ready" || first.Labels.Running != "custom-running" {
				t.Errorf("repository = %#v", first)
			}
			if second.Concurrency != 3 || !second.Enabled || second.BaseBranch != "" || second.Labels.Ready != "custom-ready" {
				t.Errorf("inherited repository = %#v", second)
			}
			if len(first.Implementer.Skills) > 0 {
				first.Implementer.Skills[0] = "changed"
			}
			if cfg.Implementer.Skills[0] != "implement" || second.Implementer.Skills[0] != "implement" {
				t.Error("resolved roles share mutable skill slices")
			}
		})
	}
}
