package doctor_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/doctor"
	"github.com/rcpassos/mergeyard/internal/runner"
)

func TestInvalidConfigurationIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("implementer: {agent: unknown}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	report := doctor.Check(context.Background(), path, doctor.Options{})
	if !report.HasErrors() || len(report.Findings) != 1 || report.Findings[0].Code != "config.invalid_agent" || report.Findings[0].Severity != doctor.Error {
		t.Fatalf("invalid configuration report: %+v", report)
	}
}

type fakeCommands struct {
	t         *testing.T
	responses map[string]runner.ExecResult
	calls     []runner.ExecRequest
}

func (f *fakeCommands) Exec(ctx context.Context, req runner.ExecRequest) (runner.ExecResult, error) {
	f.calls = append(f.calls, req)
	args := req.Args
	if req.Executable == "git" && len(args) >= 2 && args[0] == "-c" && args[1] == "core.hooksPath=/dev/null" {
		args = args[2:]
	}
	key := req.Executable + " " + strings.Join(args, " ")
	if req.Executable == "git" && len(args) > 3 && args[2] == "push" {
		key = "git push probe"
	}
	if result, ok := f.responses[key]; ok {
		return result, nil
	}
	f.t.Errorf("unexpected external command: %s", key)
	return runner.ExecResult{}, fmt.Errorf("unexpected command: %s", key)
}

func TestGitProbesAreIsolatedAndNeverPushChanges(t *testing.T) {
	path, fake, opts := repositorySetup(t, "")
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "foreign.git"))
	t.Setenv("GIT_WORK_TREE", t.TempDir())
	report := doctor.Check(context.Background(), path, opts)
	if report.HasErrors() {
		t.Fatalf("probe failed: %+v", report)
	}
	seenPush := false
	for _, call := range fake.calls {
		if call.Executable != "git" {
			continue
		}
		if call.Env["GIT_DIR"] != "" || call.Env["GIT_WORK_TREE"] != "" {
			t.Fatal("Git probe can be redirected into a user's checkout by inherited environment")
		}
		if !strings.Contains(strings.Join(call.Args, " "), "core.hooksPath=/dev/null") {
			t.Fatal("Git probe could run a configured hook")
		}
		if strings.Contains(strings.Join(call.Args, " "), " push ") {
			seenPush = true
			if !strings.Contains(strings.Join(call.Args, " "), "--dry-run --no-verify --no-force") {
				t.Fatal("push probe must never update remote refs or run hooks")
			}
		}
		if call.Dir != "" {
			if _, err := os.Stat(call.Dir); !os.IsNotExist(err) {
				t.Fatalf("temporary probe repository was not removed: %v", err)
			}
		}
	}
	if !seenPush {
		t.Fatal("Git push transport was not checked")
	}
}

func repositorySetup(t *testing.T, extra string) (string, *fakeCommands, doctor.Options) {
	t.Helper()
	path, fake, opts := setup(t, "implementer: {agent: codex}\nreviewer: {agent: codex}\nrepositories: [{repo: octo/repo}]\n"+extra)
	for command, output := range map[string]string{
		"gh api --hostname github.com --method GET repos/octo/repo":                                                                   `{"permissions":{"push":true}}`,
		"gh api --hostname github.com --method GET --paginate repos/octo/repo/labels?per_page=100":                                    `[{"name":"ready-for-agent"},{"name":"agent-running"},{"name":"agent-needs-attention"}]`,
		"git ls-remote --symref -- https://github.com/octo/repo.git HEAD":                                                             "ref: refs/heads/main\tHEAD\nabcdef\tHEAD\n",
		"git check-ref-format refs/heads/main":                                                                                        "",
		"git ls-remote --exit-code --heads -- https://github.com/octo/repo.git refs/heads/main":                                       "abcdef\trefs/heads/main\n",
		"git init --bare --template= --object-format=sha1":                                                                            "",
		"git fetch --depth=1 --no-tags --no-recurse-submodules -- https://github.com/octo/repo.git refs/heads/main:refs/heads/doctor": "",
		"git ls-tree -r --name-only refs/heads/doctor":                                                                                "CLAUDE.md\n",
		"git push probe": "",
	} {
		fake.responses[command] = runner.ExecResult{Stdout: []byte(output)}
	}
	return path, fake, opts
}

func TestMisconfiguredRepositoryReportsMissingLabelsAndInstructions(t *testing.T) {
	path, fake, opts := repositorySetup(t, "")
	fake.responses["gh api --hostname github.com --method GET --paginate repos/octo/repo/labels?per_page=100"] = runner.ExecResult{Stdout: []byte(`[{"name":"ready-for-agent"}]`)}
	report := doctor.Check(context.Background(), path, opts)
	requireFinding(t, report, doctor.Error, "github.label_missing", "octo/repo")
	requireFinding(t, report, doctor.Warning, "harness.instructions_missing", "octo/repo")
	if !report.HasErrors() {
		t.Fatal("missing labels must fail doctor")
	}
}

func setup(t *testing.T, extra string) (string, *fakeCommands, doctor.Options) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := fmt.Sprintf("port: %d\nworkspace: %q\n", port, filepath.Join(t.TempDir(), "workspace")) + extra
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeCommands{t: t, responses: map[string]runner.ExecResult{}}
	for command, output := range map[string]string{
		"git --version":                        "git version 2.50.0",
		"tmux -V":                              "tmux 3.5a",
		"gh --version":                         "gh version 2.70.0",
		"gh auth status --hostname github.com": "Logged in",
		"claude --version":                     "2.1.277 (Claude Code)",
		"claude auth status":                   `{"loggedIn":true}`,
		"codex --version":                      "codex-cli 0.156.1",
		"codex login status":                   "Logged in using ChatGPT",
	} {
		fake.responses[command] = runner.ExecResult{Stdout: []byte(output)}
	}
	return path, fake, doctor.Options{Executor: fake, EffectiveUID: func() int { return 1000 }}
}

func requireFinding(t *testing.T, report doctor.Report, severity doctor.Severity, code, scope string) {
	t.Helper()
	for _, finding := range report.Findings {
		if finding.Severity == severity && finding.Code == code && finding.Scope == scope {
			return
		}
	}
	t.Fatalf("missing %s %s (%s) in %+v", severity, code, scope, report)
}

func TestMissingMachineToolsAreErrors(t *testing.T) {
	for _, tc := range []struct{ command, scope, code string }{
		{"git --version", "git", "internal.tool_missing"},
		{"tmux -V", "tmux", "internal.tool_missing"},
		{"gh --version", "gh", "internal.tool_missing"},
		{"claude --version", "claude", "harness.not_found"},
		{"codex --version", "codex", "harness.not_found"},
	} {
		t.Run(tc.scope, func(t *testing.T) {
			path, fake, opts := setup(t, "reviewer: {agent: codex}\n")
			fake.responses[tc.command] = runner.ExecResult{ExitCode: 127, Stderr: []byte("not found")}
			report := doctor.Check(context.Background(), path, opts)
			requireFinding(t, report, doctor.Error, tc.code, tc.scope)
			if !report.HasErrors() {
				t.Fatal("missing tool must fail doctor")
			}
		})
	}
}

func TestHarnessVersions(t *testing.T) {
	for _, tc := range []struct {
		agent, output string
		severity      doctor.Severity
		code          string
	}{
		{"claude", "2.1.276 (Claude Code)", doctor.Error, "harness.version_unsupported"},
		{"codex", "codex-cli 0.156.0", doctor.Error, "harness.version_unsupported"},
		{"codex", "codex-cli 0.156.1-rc.1", doctor.Error, "harness.version_unsupported"},
		{"claude", "Claude development build", doctor.Unverifiable, "harness.version_unverifiable"},
	} {
		t.Run(tc.output, func(t *testing.T) {
			path, fake, opts := setup(t, "reviewer: {agent: codex}\n")
			fake.responses[tc.agent+" --version"] = runner.ExecResult{Stdout: []byte(tc.output)}
			report := doctor.Check(context.Background(), path, opts)
			requireFinding(t, report, tc.severity, tc.code, tc.agent)
		})
	}
}

func TestLoginFailuresAreErrors(t *testing.T) {
	for _, tc := range []struct{ command, scope, code string }{
		{"gh auth status --hostname github.com", "gh", "github.not_logged_in"},
		{"claude auth status", "claude", "harness.not_logged_in"},
		{"codex login status", "codex", "harness.not_logged_in"},
	} {
		t.Run(tc.scope, func(t *testing.T) {
			path, fake, opts := setup(t, "reviewer: {agent: codex}\n")
			fake.responses[tc.command] = runner.ExecResult{ExitCode: 1}
			requireFinding(t, doctor.Check(context.Background(), path, opts), doctor.Error, tc.code, tc.scope)
		})
	}
}

func TestRootWithBypassPermissionsIsAnError(t *testing.T) {
	path, _, opts := setup(t, "")
	opts.EffectiveUID = func() int { return 0 }
	requireFinding(t, doctor.Check(context.Background(), path, opts), doctor.Error, "harness.root_bypass_permissions", "claude")
}

func TestHealthyMachineHasNoFindings(t *testing.T) {
	path, _, opts := setup(t, "reviewer: {agent: codex}\n")
	report := doctor.Check(context.Background(), path, opts)
	if len(report.Findings) != 0 {
		t.Fatalf("healthy machine: %+v", report)
	}
}

func setConfig(t *testing.T, path string, field string, value any) {
	t.Helper()
	_, doc, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Set(strings.Split(field, "."), value); err != nil {
		t.Fatal(err)
	}
	if err := doc.Write(path); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceAndPortChecks(t *testing.T) {
	t.Run("dangling workspace symlink", func(t *testing.T) {
		path, _, opts := setup(t, "")
		link := filepath.Join(t.TempDir(), "workspace")
		if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), link); err != nil {
			t.Fatal(err)
		}
		setConfig(t, path, "workspace", link)
		requireFinding(t, doctor.Check(context.Background(), path, opts), doctor.Error, "workspace.not_writable", link)
	})
	t.Run("workspace path is a file", func(t *testing.T) {
		path, _, opts := setup(t, "")
		setConfig(t, path, "workspace", path)
		requireFinding(t, doctor.Check(context.Background(), path, opts), doctor.Error, "workspace.not_writable", path)
	})
	t.Run("occupied dashboard port", func(t *testing.T) {
		path, _, opts := setup(t, "")
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		port := listener.Addr().(*net.TCPAddr).Port
		setConfig(t, path, "port", port)
		requireFinding(t, doctor.Check(context.Background(), path, opts), doctor.Error, "config.port_unavailable", fmt.Sprintf("127.0.0.1:%d", port))
	})
	t.Run("invalid dashboard port", func(t *testing.T) {
		path, _, opts := setup(t, "")
		setConfig(t, path, "port", 65536)
		requireFinding(t, doctor.Check(context.Background(), path, opts), doctor.Error, "config.invalid_port", "port")
	})
	t.Run("safe probe does not create workspace", func(t *testing.T) {
		path, _, opts := setup(t, "")
		cfg, _, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		report := doctor.Check(context.Background(), path, opts)
		if report.HasErrors() {
			t.Fatalf("writable workspace: %+v", report)
		}
		if _, err := os.Stat(cfg.Workspace); !os.IsNotExist(err) {
			t.Fatalf("doctor created workspace: %v", err)
		}
	})
}

func TestRequestedCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name, help string
		severity   doctor.Severity
		code       string
		exit       int
	}{
		{"missing model flag", "--effort", doctor.Error, "harness.capability_unsupported", 0},
		{"help cannot run", "", doctor.Unverifiable, "harness.capabilities_unverifiable", 1},
		{"supported", "--model <model> --effort <effort>", "", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, fake, opts := setup(t, "implementer: {model: custom, effort: max}\n")
			fake.responses["claude --help"] = runner.ExecResult{Stdout: []byte(tc.help), ExitCode: tc.exit}
			report := doctor.Check(context.Background(), path, opts)
			if tc.code != "" {
				requireFinding(t, report, tc.severity, tc.code, "implementer")
			} else if len(report.Findings) != 0 {
				t.Fatalf("supported capabilities: %+v", report)
			}
		})
	}
}

func TestRoleSkills(t *testing.T) {
	for _, tc := range []struct {
		name, agent, skill, files, personal string
		severity                            doctor.Severity
		code                                string
	}{
		{"missing Codex skill", "codex", "missing-doctor-test", "AGENTS.md\n", "", doctor.Error, "harness.skill_missing"},
		{"repository Codex skill", "codex", "review", "AGENTS.md\n.agents/skills/review/SKILL.md\n", "", "", ""},
		{"personal Codex skill", "codex", "review", "AGENTS.md\n", ".agents/skills/review/SKILL.md", "", ""},
		{"repository Claude skill", "claude", "review", "CLAUDE.md\n.claude/skills/review/SKILL.md\n", "", "", ""},
		{"personal Claude skill", "claude", "review", "CLAUDE.md\n", ".claude/skills/review/SKILL.md", "", ""},
		{"plugin Claude skill", "claude", "plugin:review", "CLAUDE.md\n", "", doctor.Unverifiable, "harness.skill_unverifiable"},
		{"path traversal skill", "codex", "../other", "AGENTS.md\n", "", doctor.Error, "harness.skill_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, fake, opts := repositorySetup(t, "")
			setConfig(t, path, "repositories.0.implementer", map[string]any{"agent": tc.agent, "skills": []string{tc.skill}})
			setConfig(t, path, "repositories.0.reviewer.agent", tc.agent)
			fake.responses["git ls-tree -r --name-only refs/heads/doctor"] = runner.ExecResult{Stdout: []byte(tc.files)}
			if tc.personal != "" {
				file := filepath.Join(os.Getenv("HOME"), tc.personal)
				if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte("# test skill\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			report := doctor.Check(context.Background(), path, opts)
			if tc.code != "" {
				requireFinding(t, report, tc.severity, tc.code, "octo/repo implementer")
			} else if len(report.Findings) != 0 {
				t.Fatalf("available role skill: %+v", report)
			}
		})
	}
}

func TestRepositoryCheckFailures(t *testing.T) {
	for _, tc := range []struct {
		name, command, output string
		exit                  int
		severity              doctor.Severity
		code                  string
	}{
		{"origin inaccessible", "git ls-remote --symref -- https://github.com/octo/repo.git HEAD", "", 1, doctor.Error, "git.origin_inaccessible"},
		{"default branch absent", "git ls-remote --symref -- https://github.com/octo/repo.git HEAD", "", 0, doctor.Error, "git.base_branch"},
		{"invalid base branch", "git check-ref-format refs/heads/main", "", 1, doctor.Error, "git.base_branch"},
		{"base branch absent", "git ls-remote --exit-code --heads -- https://github.com/octo/repo.git refs/heads/main", "", 2, doctor.Error, "git.base_branch"},
		{"fetch failure", "git fetch --depth=1 --no-tags --no-recurse-submodules -- https://github.com/octo/repo.git refs/heads/main:refs/heads/doctor", "", 1, doctor.Error, "git.fetch"},
		{"push transport failure", "git push probe", "", 1, doctor.Error, "git.push_access"},
		{"push denied", "gh api --hostname github.com --method GET repos/octo/repo", `{"permissions":{"push":false}}`, 0, doctor.Error, "github.push_forbidden"},
		{"push permission omitted", "gh api --hostname github.com --method GET repos/octo/repo", `{}`, 0, doctor.Unverifiable, "github.push_unverifiable"},
		{"permissions request failed", "gh api --hostname github.com --method GET repos/octo/repo", "", 1, doctor.Unverifiable, "github.push_unverifiable"},
		{"invalid metadata", "gh api --hostname github.com --method GET repos/octo/repo", `null`, 0, doctor.Error, "github.invalid_response"},
		{"label request failed", "gh api --hostname github.com --method GET --paginate repos/octo/repo/labels?per_page=100", "", 1, doctor.Error, "github.labels_check_failed"},
		{"invalid labels", "gh api --hostname github.com --method GET --paginate repos/octo/repo/labels?per_page=100", `{}`, 0, doctor.Error, "github.invalid_response"},
		{"tree unavailable", "git ls-tree -r --name-only refs/heads/doctor", "", 1, doctor.Unverifiable, "git.tree_unverifiable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, fake, opts := repositorySetup(t, "")
			fake.responses[tc.command] = runner.ExecResult{Stdout: []byte(tc.output), ExitCode: tc.exit}
			requireFinding(t, doctor.Check(context.Background(), path, opts), tc.severity, tc.code, "octo/repo")
		})
	}
}

func TestRepositoryLabelOverridesAcrossPages(t *testing.T) {
	path, fake, opts := repositorySetup(t, "")
	setConfig(t, path, "repositories.0.labels", map[string]string{"ready": "work", "running": "busy", "needs_attention": "help"})
	fake.responses["gh api --hostname github.com --method GET --paginate repos/octo/repo/labels?per_page=100"] = runner.ExecResult{Stdout: []byte(`[{"name":"WORK"}][{"name":"busy"},{"name":"help"}]`)}
	fake.responses["git ls-tree -r --name-only refs/heads/doctor"] = runner.ExecResult{Stdout: []byte("AGENTS.md\n")}
	report := doctor.Check(context.Background(), path, opts)
	if len(report.Findings) != 0 {
		t.Fatalf("configured labels on separate pages: %+v", report)
	}
}

func TestClaudeInstructionWarningBelowMinimumVersion(t *testing.T) {
	path, fake, opts := repositorySetup(t, "")
	setConfig(t, path, "repositories.0.implementer.agent", "claude")
	fake.responses["claude --version"] = runner.ExecResult{Stdout: []byte("2.1.276 (Claude Code)")}
	fake.responses["git ls-tree -r --name-only refs/heads/doctor"] = runner.ExecResult{Stdout: []byte("AGENTS.md\n")}
	report := doctor.Check(context.Background(), path, opts)
	requireFinding(t, report, doctor.Warning, "harness.instructions_missing", "octo/repo")
	requireFinding(t, report, doctor.Error, "harness.version_unsupported", "claude")
}

func TestCustomHarnessExecutableAndCodexCapabilities(t *testing.T) {
	path, fake, opts := setup(t, "implementer: {agent: codex, model: arbitrary, effort: arbitrary}\nreviewer: {agent: codex}\n")
	setConfig(t, path, "agents.codex.executable", "/custom/codex cli")
	fake.responses["/custom/codex cli --version"] = runner.ExecResult{Stdout: []byte("codex-cli 0.156.1")}
	fake.responses["/custom/codex cli login status"] = runner.ExecResult{}
	fake.responses["/custom/codex cli --help"] = runner.ExecResult{Stdout: []byte("--model <MODEL> --config <KEY=VALUE>")}
	report := doctor.Check(context.Background(), path, opts)
	if len(report.Findings) != 0 {
		t.Fatalf("custom executable and capability passthrough: %+v", report)
	}
	fake.responses["/custom/codex cli --help"] = runner.ExecResult{Stdout: []byte("--model <MODEL>")}
	requireFinding(t, doctor.Check(context.Background(), path, opts), doctor.Error, "harness.capability_unsupported", "implementer")
}
