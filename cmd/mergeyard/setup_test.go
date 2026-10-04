package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
)

func setupTools(t *testing.T) (home, log string) {
	t.Helper()
	home, tools := t.TempDir(), t.TempDir()
	log = filepath.Join(home, "commands")
	t.Setenv("HOME", home)
	t.Setenv("PATH", tools)
	t.Setenv("SETUP_LOG", log)
	for name, script := range map[string]string{
		"git":    "#!/bin/sh\nexit 0\n",
		"tmux":   "#!/bin/sh\nexit 0\n",
		"claude": "#!/bin/sh\ncase \"$1\" in --version) echo '2.1.277';; esac\n",
		"gh": `#!/bin/sh
printf '%s\n' "$*" >> "$SETUP_LOG"
case "$*" in
 *'/labels?'*) printf '%s\n' '[{"name":"READY-FOR-AGENT"}]' '[{"name":"agent-running"}]';;
 *'repos/octo/repo'*) echo '{"permissions":{"push":true}}';;
esac
`,
	} {
		if err := os.WriteFile(filepath.Join(tools, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return home, log
}

func setupCLI(t *testing.T, input string, args ...string) (int, string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(path, []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	old := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = old }()
	return runCLI(args...)
}

func TestInitWritesConfigCreatesOnlyMissingLabelsAndRunsDoctor(t *testing.T) {
	home, log := setupTools(t)
	path := filepath.Join(home, "config.yaml")
	code, out, errOut := setupCLI(t, "claude\nclaude\nocto/repo\ny\n", "init", "--config", path)
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("config was not written: %v; exit=%d out=%q err=%q", err, code, out, errOut)
	}
	if len(cfg.Repositories) != 1 || cfg.Repositories[0].Repo != "octo/repo" || cfg.Implementer.Agent != "claude" || cfg.Reviewer.Agent != "claude" {
		t.Fatalf("setup config: %+v", cfg)
	}
	if !strings.Contains(out, "git: installed") || !strings.Contains(out, "codex: missing") || !strings.Contains(out, "error:\n") || !strings.Contains(out, "git.base_branch") || code != 1 {
		t.Fatalf("detection/doctor result: exit=%d out=%q err=%q", code, out, errOut)
	}
	commands, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(commands), "-- agent-needs-attention") || strings.Contains(string(commands), "-- ready-for-agent") || strings.Contains(string(commands), "-- agent-running") || strings.Contains(string(commands), "--force") {
		t.Fatalf("label mutations: %s", commands)
	}
}

func TestInitDeclinesOverwriteWithoutChangingConfigOrGitHub(t *testing.T) {
	home, log := setupTools(t)
	path := filepath.Join(home, "config.yaml")
	original := "# keep exactly\nrepositories: [{repo: octo/original}]\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := setupCLI(t, "n\n", "init", "--config", path)
	after, err := os.ReadFile(path)
	if err != nil || string(after) != original || code != 0 || errOut != "" || !strings.Contains(out, "Overwrite") {
		t.Fatalf("overwrite refusal: exit=%d out=%q err=%q contents=%q read=%v", code, out, errOut, after, err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatalf("GitHub was contacted after cancellation: %v", err)
	}
}

func TestRepoAddPreservesUserConfigAndCanDeclineLabels(t *testing.T) {
	home, log := setupTools(t)
	path := filepath.Join(home, "config.yaml")
	original := "# user settings\nport: 7444\nfuture_setting: kept\nrepositories:\n  - repo: octo/original # existing\n    concurrency: 2\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := setupCLI(t, "n\n", "repo", "add", "octo/repo", "--config", path)
	cfg, _, err := config.Load(path)
	if err != nil || code != 0 || errOut != "" || len(cfg.Repositories) != 2 || cfg.Repositories[1].Repo != "octo/repo" || cfg.Repositories[0].Concurrency != 2 || cfg.Port != 7444 {
		t.Fatalf("repo add: exit=%d out=%q err=%q config=%+v load=%v", code, out, errOut, cfg, err)
	}
	after, _ := os.ReadFile(path)
	for _, text := range []string{"# user settings", "# existing", "future_setting: kept"} {
		if !strings.Contains(string(after), text) {
			t.Fatalf("lost user content %q: %s", text, after)
		}
	}
	commands, _ := os.ReadFile(log)
	if !strings.Contains(string(commands), "repos/octo/repo") || strings.Contains(string(commands), "label create") {
		t.Fatalf("access/label commands: %s", commands)
	}
}

func TestInitSelectsInstalledAgentsAndCreatesDefaultHomeConfig(t *testing.T) {
	home, _ := setupTools(t)
	t.Chdir(t.TempDir())
	tools := os.Getenv("PATH")
	if err := os.WriteFile(filepath.Join(tools, "codex"), []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'codex-cli 0.134.0'; fi\n"), 0700); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := setupCLI(t, "not-installed\ncodex\nclaude\ninvalid\nocto/repo\nn\n", "init")
	path := filepath.Join(home, ".config", "mergeyard", "config.yaml")
	cfg, _, err := config.Load("")
	if err != nil || cfg.Implementer.Agent != "codex" || cfg.Reviewer.Agent != "claude" || len(cfg.Repositories) != 1 {
		t.Fatalf("init choices: exit=%d out=%q err=%q config=%+v load=%v", code, out, errOut, cfg, err)
	}
	if !strings.Contains(out, "Choose an installed agent") || !strings.Contains(out, "Enter a repository as owner/repo") {
		t.Fatalf("missing validation prompts: %s", out)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("new config permissions: %v, %v", info, err)
	}
}

func TestInitCannotSelectMissingAgent(t *testing.T) {
	home, _ := setupTools(t)
	path := filepath.Join(home, "config.yaml")
	_, out, errOut := setupCLI(t, "codex\n", "init", "--config", path)
	if !strings.Contains(out, "Implementer agent (claude)") || !strings.Contains(out, "Choose an installed agent") || !strings.Contains(errOut, "read answer") {
		t.Fatalf("missing agent selection: out=%q err=%q", out, errOut)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("incomplete setup wrote config: %v", err)
	}
}

func TestSetupFailuresLeaveConfigurationAndLabelsUntouched(t *testing.T) {
	for _, tc := range []struct {
		name                                string
		args                                []string
		input, existing, tool, script, want string
	}{
		{name: "missing tool", args: []string{"init"}, tool: "tmux", want: "internal.tool_missing"},
		{name: "no agent", args: []string{"init"}, tool: "claude", want: "harness.not_found"},
		{name: "EOF before label consent", args: []string{"init"}, input: "\n\nocto/repo\n", want: "read answer"},
		{name: "invalid repository", args: []string{"repo", "add", "../bad"}, existing: "{}", want: "config.invalid_repo"},
		{name: "duplicate repository", args: []string{"repo", "add", "OCTO/REPO"}, existing: "repositories: [{repo: octo/repo}]", want: "config.duplicate_repo"},
		{name: "inaccessible repository", args: []string{"repo", "add", "octo/repo"}, existing: "{}", tool: "gh", script: "#!/bin/sh\nexit 1\n", want: "github.command_failed"},
		{name: "invalid labels response", args: []string{"repo", "add", "octo/repo"}, input: "y\n", existing: "{}", tool: "gh", script: "#!/bin/sh\ncase \"$*\" in *'/labels?'*) echo 'null';; *) echo '{}';; esac\n", want: "github.invalid_response"},
		{name: "label creation denied", args: []string{"repo", "add", "octo/repo"}, input: "y\n", existing: "{}", tool: "gh", script: "#!/bin/sh\ncase \"$*\" in *'/labels?'*) echo '[]';; 'label create'*) exit 1;; *) echo '{}';; esac\n", want: "github.command_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, log := setupTools(t)
			path := filepath.Join(home, "config.yaml")
			if tc.existing != "" {
				if err := os.WriteFile(path, []byte(tc.existing), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.tool != "" {
				toolPath := filepath.Join(os.Getenv("PATH"), tc.tool)
				if tc.script == "" {
					if err := os.Remove(toolPath); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(toolPath, []byte(tc.script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			args := append(append([]string{}, tc.args...), "--config", path)
			code, out, errOut := setupCLI(t, tc.input, args...)
			if code != 1 || !strings.Contains(errOut, tc.want) {
				t.Fatalf("failure result: exit=%d out=%q err=%q", code, out, errOut)
			}
			after, err := os.ReadFile(path)
			if tc.existing == "" {
				if !os.IsNotExist(err) {
					t.Fatalf("failure wrote config: %v", err)
				}
			} else if err != nil || string(after) != tc.existing {
				t.Fatalf("failure changed config: contents=%q err=%v", after, err)
			}
			commands, _ := os.ReadFile(log)
			if strings.Contains(string(commands), "label create") {
				t.Fatalf("failure changed labels: %s", commands)
			}
		})
	}
}

func TestRepoAddCreatesConfiguredLabels(t *testing.T) {
	home, log := setupTools(t)
	path := filepath.Join(home, "config.yaml")
	data := "labels: {ready: queue, running: working, needs_attention: help}\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := setupCLI(t, "y\n", "repo", "--config", path, "add", "octo/repo")
	if code != 0 || errOut != "" {
		t.Fatalf("repo add: exit=%d out=%q err=%q", code, out, errOut)
	}
	commands, _ := os.ReadFile(log)
	for _, name := range []string{"queue", "working", "help"} {
		if !strings.Contains(string(commands), "label create --repo github.com/octo/repo") || !strings.Contains(string(commands), "-- "+name+"\n") {
			t.Fatalf("missing configured label %s: %s", name, commands)
		}
	}
}

func TestInitUpdatesLocalConfigPreservesSettingsAndPassesDoctor(t *testing.T) {
	home, _ := setupTools(t)
	t.Chdir(t.TempDir())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	data := fmt.Sprintf("# keep preferences\nport: %d\nfuture_setting: kept\nagents: {claude: {permission_mode: acceptEdits}}\nrepositories: [{repo: octo/repo}]\n", port)
	if err := os.WriteFile("mergeyard.yaml", []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(home, ".config", "mergeyard", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(global), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(global, []byte("untouched global config"), 0600); err != nil {
		t.Fatal(err)
	}
	tools := os.Getenv("PATH")
	git := `#!/bin/sh
case "$*" in
 *'ls-remote --symref'*) printf 'ref: refs/heads/main\tHEAD\n';;
 *'ls-remote --exit-code'*) printf '0123456789012345678901234567890123456789\trefs/heads/main\n';;
 *'cat-file'*) while IFS= read -r line; do echo blob; done;;
esac
`
	gh := `#!/bin/sh
case "$*" in
 *'/labels?'*) echo '[{"name":"ready-for-agent"},{"name":"agent-running"},{"name":"agent-needs-attention"}]';;
 *'repos/octo/repo'*) echo '{"permissions":{"push":true}}';;
esac
`
	for name, script := range map[string]string{"git": git, "gh": gh} {
		if err := os.WriteFile(filepath.Join(tools, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	code, out, errOut := setupCLI(t, "y\n\n\nocto/repo\nn\n", "init")
	if code != 0 || errOut != "" || !strings.Contains(out, "error:\n  none\n") || !strings.Contains(out, "config.unknown_field") {
		t.Fatalf("healthy setup: exit=%d out=%q err=%q", code, out, errOut)
	}
	cfg, _, err := config.Load("")
	if err != nil || cfg.Port != port || len(cfg.Repositories) != 1 {
		t.Fatalf("existing settings: %+v, %v", cfg, err)
	}
	after, _ := os.ReadFile("mergeyard.yaml")
	if !strings.Contains(string(after), "# keep preferences") || !strings.Contains(string(after), "future_setting: kept") {
		t.Fatalf("lost user settings: %s", after)
	}
	globalAfter, _ := os.ReadFile(global)
	if string(globalAfter) != "untouched global config" {
		t.Fatalf("init changed unselected config: %s", globalAfter)
	}
}

func TestSetupInterruptsPendingPromptWithoutSaving(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mergeyard")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	for _, command := range []string{"init", "repo add"} {
		t.Run(command, func(t *testing.T) {
			home, _ := setupTools(t)
			path := filepath.Join(home, "config.yaml")
			original := "{}\n"
			input := ""
			args := []string{"init", "--config", path}
			if command == "repo add" {
				if err := os.WriteFile(path, []byte(original), 0600); err != nil {
					t.Fatal(err)
				}
				args = []string{"repo", "add", "octo/repo", "--config", path}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, args...)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Process.Kill()
			ready := make(chan error, 1)
			go func() {
				reader := bufio.NewReader(stdout)
				for {
					b, err := reader.ReadByte()
					if err != nil {
						ready <- err
						return
					}
					input += string(b)
					if strings.HasSuffix(input, "]: ") {
						ready <- nil
						return
					}
				}
			}()
			select {
			case err := <-ready:
				if err != nil {
					t.Fatalf("prompt did not appear: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("prompt did not appear")
			}
			if err := cmd.Process.Signal(os.Interrupt); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			select {
			case err := <-exited:
				if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
					t.Fatalf("interrupted CLI exit: %v", err)
				}
			case <-time.After(2 * time.Second):
				cmd.Process.Kill()
				<-exited
				t.Fatal("interrupted prompt kept waiting for input")
			}
			after, err := os.ReadFile(path)
			if command == "init" {
				if !os.IsNotExist(err) {
					t.Fatalf("interrupted init saved config: %v", err)
				}
			} else if err != nil || string(after) != original {
				t.Fatalf("interrupted repo add changed config: %q %v", after, err)
			}
		})
	}
}

func TestInitUpdatesAliasedRolesIndependently(t *testing.T) {
	for _, tc := range []struct{ name, implementer, reviewer string }{
		{"unchanged agents", "claude", "claude"},
		{"change implementer", "codex", "claude"},
		{"change reviewer", "claude", "codex"},
		{"change both", "codex", "codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, _ := setupTools(t)
			path := filepath.Join(home, "config.yaml")
			if err := os.WriteFile(filepath.Join(os.Getenv("PATH"), "codex"), []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'codex-cli 0.134.0'; fi\n"), 0700); err != nil {
				t.Fatal(err)
			}
			original := `# shared roles
implementer: &role
  agent: claude # keep choice comment
  max_attempts: 2
  future_role_option: kept
reviewer: *role # keep reviewer comment
future_setting: &other preserved
future_alias: *other
repositories:
  - repo: octo/repo
    implementer: *role
`
			if err := os.WriteFile(path, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			input := "y\n" + tc.implementer + "\n" + tc.reviewer + "\nocto/repo\nn\n"
			code, out, errOut := setupCLI(t, input, "init", "--config", path)
			cfg, _, err := config.Load(path)
			if err != nil || errOut != "" || !strings.Contains(out, "Config saved") || cfg.Implementer.Agent != tc.implementer || cfg.Reviewer.Agent != tc.reviewer {
				t.Fatalf("aliased init: exit=%d out=%q stderr=%q config=%+v load=%v", code, out, errOut, cfg, err)
			}
			if cfg.Implementer.MaxAttempts != 2 || cfg.Reviewer.MaxAttempts != 2 || cfg.Repositories[0].Implementer.Agent != "claude" {
				t.Fatalf("init changed preserved role settings: %+v", cfg)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, text := range []string{"# shared roles", "# keep choice comment", "# keep reviewer comment", "future_role_option: kept", "future_alias: *other"} {
				if !strings.Contains(string(after), text) {
					t.Fatalf("lost user content %q: %s", text, after)
				}
			}
			if tc.implementer == "claude" && tc.reviewer == "claude" && !strings.Contains(string(after), "reviewer: *role") {
				t.Fatalf("unchanged setup expanded alias: %s", after)
			}
		})
	}
}

func TestRepoAddCreatesLabelsWhoseNamesLookLikeOptions(t *testing.T) {
	home, _ := setupTools(t)
	path := filepath.Join(home, "config.yaml")
	data := "labels: {ready: '--help', running: '-R', needs_attention: '--description'}\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	created := filepath.Join(home, "created-labels")
	t.Setenv("SETUP_CREATED_LABELS", created)
	// Model the CLI parser at the process boundary: --help returns success
	// without a write, while operands after -- are treated as literal names.
	gh := `#!/bin/sh
case "$1 $2" in
 'api --hostname')
  case "$*" in *'/labels?'*) echo '[]';; *) echo '{}';; esac
  exit 0;;
 'label create') shift 2;;
 *) exit 1;;
esac
while [ "$#" -gt 0 ]; do
 case "$1" in
  --help) exit 0;;
  --repo|--color|--description) shift 2;;
  --) shift; [ "$#" -eq 1 ] || exit 2
      printf '%s\n' "$1" >> "$SETUP_CREATED_LABELS"; exit 0;;
  *) shift;;
 esac
done
`
	if err := os.WriteFile(filepath.Join(os.Getenv("PATH"), "gh"), []byte(gh), 0700); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := setupCLI(t, "y\n", "repo", "add", "octo/repo", "--config", path)
	after, err := os.ReadFile(created)
	if code != 0 || errOut != "" || err != nil || string(after) != "--help\n-R\n--description\n" {
		t.Fatalf("literal label creation: exit=%d out=%q stderr=%q created=%q read=%v", code, out, errOut, after, err)
	}
	cfg, _, err := config.Load(path)
	if err != nil || len(cfg.Repositories) != 1 {
		t.Fatalf("repo add config: %+v, %v", cfg, err)
	}
}

func TestInitUpdatesScalarAgentAliasesWithoutChangingSharedValues(t *testing.T) {
	home, _ := setupTools(t)
	path := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(filepath.Join(os.Getenv("PATH"), "codex"), []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'codex-cli 0.134.0'; fi\n"), 0700); err != nil {
		t.Fatal(err)
	}
	original := `future_agent: &agent claude
implementer: {agent: *agent, max_attempts: 2}
reviewer: {agent: *agent, max_attempts: 2}
repositories:
  - repo: octo/repo
    implementer: {agent: *agent}
`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	_, out, errOut := setupCLI(t, "y\ncodex\nclaude\nocto/repo\nn\n", "init", "--config", path)
	cfg, _, err := config.Load(path)
	if err != nil || errOut != "" || !strings.Contains(out, "Config saved") || cfg.Implementer.Agent != "codex" || cfg.Reviewer.Agent != "claude" || cfg.Repositories[0].Implementer.Agent != "claude" {
		t.Fatalf("scalar aliases: out=%q stderr=%q config=%+v load=%v", out, errOut, cfg, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(after), "future_agent: &agent claude") {
		t.Fatalf("changed shared value: %s %v", after, err)
	}
}
