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

// The CLI keeps its documented error/warning/unverifiable output even when
// the report also carries passes for the dashboard.
func TestDoctorWriteOmitsPasses(t *testing.T) {
	report := doctor.Report{
		Findings: []doctor.Finding{{Severity: doctor.Warning, Code: "config.unknown_field", Scope: "port", Message: "unknown field is ignored"}},
		Passes:   []doctor.Finding{{Severity: doctor.Pass, Code: "config.valid", Scope: "configuration", Message: "configuration loaded and validated"}},
	}
	var out strings.Builder
	if err := report.Write(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "config.unknown_field") || strings.Contains(out.String(), "config.valid") || strings.Contains(out.String(), "configuration loaded") {
		t.Fatalf("doctor output includes passes: %q", out.String())
	}
}

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
	t               *testing.T
	responses       map[string]runner.ExecResult
	calls           []runner.ExecRequest
	repositoryFiles map[string]string
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
	if key == "git cat-file --batch-check=%(objecttype) --follow-symlinks" {
		input, err := os.ReadFile(req.StdinPath)
		if err != nil {
			return runner.ExecResult{}, err
		}
		var output strings.Builder
		for _, query := range strings.Split(strings.TrimSuffix(string(input), "\n"), "\n") {
			path := strings.TrimPrefix(query, "refs/heads/doctor:")
			if result, found := f.repositoryFiles[path]; found {
				output.WriteString(result)
			} else {
				fmt.Fprintf(&output, "%s missing\n", query)
			}
		}
		return runner.ExecResult{Stdout: []byte(output.String())}, nil
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
		// The machine-configuration probe runs outside any probe repository.
		if call.Dir != "" && !strings.Contains(strings.Join(call.Args, " "), "config --get core.sshCommand") {
			if _, err := os.Stat(call.Dir); !os.IsNotExist(err) {
				t.Fatalf("temporary probe repository was not removed: %v", err)
			}
		}
	}
	if !seenPush {
		t.Fatal("Git push transport was not checked")
	}
}

func TestSSHProbesAreNonInteractiveUnlessTheMachineChooses(t *testing.T) {
	const managed = "ssh -o BatchMode=yes"
	cases := []struct{ name, configured, want string }{
		{"managed default", "", managed},
		{"configured core.sshCommand wins", "ssh -i from-config", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, fake, opts := repositorySetup(t, "")
			t.Setenv("GIT_SSH", "")
			t.Setenv("GIT_SSH_COMMAND", "")
			fake.responses["git config --get core.sshCommand"] = runner.ExecResult{Stdout: []byte(tc.configured)}
			report := doctor.Check(context.Background(), path, opts)
			if report.HasErrors() {
				t.Fatalf("probe failed: %+v", report)
			}
			checked := 0
			for _, call := range fake.calls {
				if call.Executable != "git" || strings.Contains(strings.Join(call.Args, " "), "config --get core.sshCommand") {
					continue
				}
				checked++
				if tc.want == "" && call.Env["GIT_SSH_COMMAND"] != "" {
					t.Fatalf("managed default replaced core.sshCommand: %q", call.Env["GIT_SSH_COMMAND"])
				}
				if tc.want != "" && !strings.HasPrefix(call.Env["GIT_SSH_COMMAND"], tc.want) {
					t.Fatalf("SSH probes are not non-interactive: %q", call.Env["GIT_SSH_COMMAND"])
				}
			}
			if checked == 0 {
				t.Fatal("no git probe ran")
			}
		})
	}
}

func repositorySetup(t *testing.T, extra string) (string, *fakeCommands, doctor.Options) {
	t.Helper()
	path, fake, opts := setup(t, "implementer: {agent: codex}\nreviewer: {agent: codex}\nrepositories: [{repo: octo/repo}]\n"+extra)
	fake.repositoryFiles = fakeRegularRepositoryFiles("CLAUDE.md\n")
	for command, output := range map[string]string{
		"gh api --hostname github.com --method GET repos/octo/repo":                                                                   `{"permissions":{"push":true}}`,
		"gh api --hostname github.com --method GET --paginate repos/octo/repo/labels?per_page=100":                                    `[{"name":"ready-for-agent"},{"name":"agent-running"},{"name":"agent-needs-attention"}]`,
		"git ls-remote --symref -- https://github.com/octo/repo.git HEAD":                                                             "ref: refs/heads/main\tHEAD\nabcdef\tHEAD\n",
		"git check-ref-format refs/heads/main":                                                                                        "",
		"git ls-remote --exit-code --heads -- https://github.com/octo/repo.git refs/heads/main":                                       "abcdef\trefs/heads/main\n",
		"git init --bare --template= --object-format=sha1":                                                                            "",
		"git fetch --depth=1 --no-tags --no-recurse-submodules -- https://github.com/octo/repo.git refs/heads/main:refs/heads/doctor": "",
		"git push probe": "",
	} {
		fake.responses[command] = runner.ExecResult{Stdout: []byte(output)}
	}
	return path, fake, opts
}

func fakeRegularRepositoryFiles(paths string) map[string]string {
	files := make(map[string]string)
	for _, path := range strings.Split(strings.TrimSuffix(paths, "\n"), "\n") {
		files[path] = "blob\n"
	}
	return files
}

func TestRepositoryScopedFinding(t *testing.T) {
	repositories := []config.Repository{{Repo: "octo/repo"}, {Repo: "octo/other"}}
	for _, scope := range []string{"octo/repo", "OCTO/REPO implementer", "octo/repo reviewer"} {
		if !(doctor.Finding{Scope: scope}).RepositoryScoped(repositories) {
			t.Fatalf("%q was not repository-scoped", scope)
		}
	}
	for _, scope := range []string{"octo", "octo/repository", "gh", "configuration"} {
		if (doctor.Finding{Scope: scope}).RepositoryScoped(repositories) {
			t.Fatalf("%q was treated as repository-scoped", scope)
		}
	}
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
	// The SSH policy must resolve the configuration, not an ambient command.
	t.Setenv("GIT_SSH", "")
	t.Setenv("GIT_SSH_COMMAND", "")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	// Healthy fixtures keep all allocatable agent blocks above the dashboard.
	agentPortBase, agentPortSize := port+1, 2
	if agentPortBase > 65534 {
		// A dashboard at the TCP ceiling instead sits beyond one large block.
		agentPortBase, agentPortSize = 1, port-1
	}
	data := fmt.Sprintf("port: %d\nagent_ports: {base: %d, size: %d}\nworkspace: %q\n", port, agentPortBase, agentPortSize, filepath.Join(t.TempDir(), "workspace")) + extra
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeCommands{t: t, responses: map[string]runner.ExecResult{}}
	for command, output := range map[string]string{
		"git --version":                        "git version 2.50.0",
		"git config --get core.sshCommand":     "",
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

func requirePass(t *testing.T, report doctor.Report, code string) {
	t.Helper()
	for _, pass := range report.Passes {
		if pass.Severity == doctor.Pass && pass.Code == code {
			return
		}
	}
	t.Fatalf("missing pass %s in %+v", code, report)
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
			// An answered reachability probe keeps a failed login an error.
			fake.responses["gh api --hostname github.com --method GET rate_limit"] = runner.ExecResult{Stdout: []byte(`{"resources":{}}`)}
			requireFinding(t, doctor.Check(context.Background(), path, opts), doctor.Error, tc.code, tc.scope)
		})
	}
}

func TestAuthenticationDuringGitHubOutageIsUnverifiable(t *testing.T) {
	path, fake, opts := setup(t, "reviewer: {agent: codex}\n")
	fake.responses["gh auth status --hostname github.com"] = runner.ExecResult{ExitCode: 1, Stderr: []byte("Failed to log in to github.com account octo\nThe token in keyring is invalid.")}
	fake.responses["gh api --hostname github.com --method GET rate_limit"] = runner.ExecResult{ExitCode: 1, Stderr: []byte(`Get "https://api.github.com/rate_limit": dial tcp: lookup api.github.com: no such host`)}
	report := doctor.Check(context.Background(), path, opts)
	if report.HasErrors() {
		t.Fatalf("unreachable GitHub must not fail doctor: %+v", report)
	}
	requireFinding(t, report, doctor.Unverifiable, "github.not_logged_in", "gh")
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
	for _, code := range []string{"config.valid", "workspace.writable", "config.port_available", "internal.tool_ready", "github.authenticated", "harness.ready", "harness.version_supported", "harness.logged_in"} {
		requirePass(t, report, code)
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
			} else {
				requirePass(t, report, "harness.capabilities_supported")
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
			fake.repositoryFiles = fakeRegularRepositoryFiles(tc.files)
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
			} else {
				requirePass(t, report, "harness.skill_ready")
			}
		})
	}
}

func TestRepositoryFileCheckFailuresAreUnverifiable(t *testing.T) {
	for _, output := range []string{"blob\n", "blob\nblob\nsymlink 9999\nshort\n", "blob\nblob\nunknown\n", "blob\nblob\nblob\nextra\n"} {
		t.Run(fmt.Sprintf("%q", output), func(t *testing.T) {
			path, fake, opts := repositorySetup(t, "")
			setConfig(t, path, "repositories.0.implementer.skills", []string{"review"})
			fake.responses["git cat-file --batch-check=%(objecttype) --follow-symlinks"] = runner.ExecResult{Stdout: []byte(output)}
			report := doctor.Check(context.Background(), path, opts)
			requireFinding(t, report, doctor.Unverifiable, "git.tree_unverifiable", "octo/repo")
			requireFinding(t, report, doctor.Unverifiable, "harness.skill_unverifiable", "octo/repo implementer")
		})
	}
}

func TestUnavailableRepositorySkillCanUsePersonalSkill(t *testing.T) {
	path, fake, opts := repositorySetup(t, "")
	setConfig(t, path, "repositories.0.implementer.skills", []string{"review"})
	fake.repositoryFiles = fakeRegularRepositoryFiles("AGENTS.md\n")
	query := "refs/heads/doctor:.agents/skills/review/SKILL.md"
	fake.repositoryFiles[".agents/skills/review/SKILL.md"] = fmt.Sprintf("dangling %d\n%s\n", len(query), query)
	file := filepath.Join(os.Getenv("HOME"), ".agents", "skills", "review", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("# Skill\n"), 0600); err != nil {
		t.Fatal(err)
	}
	report := doctor.Check(context.Background(), path, opts)
	if len(report.Findings) != 0 {
		t.Fatalf("readable personal skill fallback: %+v", report)
	}
}

func TestInstructionFilesWithUnknownClaudeVersionAreUnverifiable(t *testing.T) {
	path, fake, opts := repositorySetup(t, "")
	setConfig(t, path, "repositories.0.implementer.agent", "claude")
	setConfig(t, path, "repositories.0.reviewer.agent", "claude")
	fake.responses["claude --version"] = runner.ExecResult{Stdout: []byte("Claude development build")}
	fake.repositoryFiles = fakeRegularRepositoryFiles("AGENTS.md\n")
	report := doctor.Check(context.Background(), path, opts)
	requireFinding(t, report, doctor.Unverifiable, "harness.instructions_unverifiable", "octo/repo")
	for _, pass := range report.Passes {
		if pass.Code == "harness.instructions_match" {
			t.Fatalf("instruction check passed without a Claude version: %+v", pass)
		}
	}
}

func TestInstructionFilesMustResolveToRegularFiles(t *testing.T) {
	path, fake, opts := repositorySetup(t, "")
	fake.repositoryFiles["AGENTS.md"] = "tree\n"
	requireFinding(t, doctor.Check(context.Background(), path, opts), doctor.Warning, "harness.instructions_missing", "octo/repo")
	fake.repositoryFiles["AGENTS.md"] = "symlink 11\n../external\n"
	requireFinding(t, doctor.Check(context.Background(), path, opts), doctor.Unverifiable, "harness.instructions_unverifiable", "octo/repo")
}

func TestRepositoryCheckFailures(t *testing.T) {
	for _, tc := range []struct {
		name, command, output string
		stderr                string
		exit                  int
		severity              doctor.Severity
		code                  string
	}{
		{"origin inaccessible", "git ls-remote --symref -- https://github.com/octo/repo.git HEAD", "", "", 1, doctor.Error, "git.origin_inaccessible"},
		{"origin unreachable", "git ls-remote --symref -- https://github.com/octo/repo.git HEAD", "", "fatal: unable to access 'https://github.com/octo/repo.git/': Could not resolve host: github.com", 128, doctor.Unverifiable, "git.origin_inaccessible"},
		{"default branch absent", "git ls-remote --symref -- https://github.com/octo/repo.git HEAD", "", "", 0, doctor.Error, "git.base_branch"},
		{"invalid base branch", "git check-ref-format refs/heads/main", "", "", 1, doctor.Error, "git.base_branch"},
		{"base branch absent", "git ls-remote --exit-code --heads -- https://github.com/octo/repo.git refs/heads/main", "", "", 2, doctor.Error, "git.base_branch"},
		{"base branch request failed", "git ls-remote --exit-code --heads -- https://github.com/octo/repo.git refs/heads/main", "", "", 1, doctor.Error, "git.base_branch"},
		{"base branch request unreachable", "git ls-remote --exit-code --heads -- https://github.com/octo/repo.git refs/heads/main", "", "fatal: unable to access 'https://github.com/octo/repo.git/': The requested URL returned error: 503", 128, doctor.Unverifiable, "git.base_branch"},
		{"fetch failure", "git fetch --depth=1 --no-tags --no-recurse-submodules -- https://github.com/octo/repo.git refs/heads/main:refs/heads/doctor", "", "", 1, doctor.Error, "git.fetch"},
		{"fetch unreachable", "git fetch --depth=1 --no-tags --no-recurse-submodules -- https://github.com/octo/repo.git refs/heads/main:refs/heads/doctor", "", "fatal: unable to access 'https://github.com/octo/repo.git/': Connection timed out", 128, doctor.Unverifiable, "git.fetch"},
		{"push transport failure", "git push probe", "", "", 1, doctor.Error, "git.push_access"},
		{"push transport unreachable", "git push probe", "", "fatal: unable to access 'https://github.com/octo/repo.git/': Failed to connect to github.com port 443", 128, doctor.Unverifiable, "git.push_access"},
		{"push denied", "gh api --hostname github.com --method GET repos/octo/repo", `{"permissions":{"push":false}}`, "", 0, doctor.Error, "github.push_forbidden"},
		{"push permission omitted", "gh api --hostname github.com --method GET repos/octo/repo", `{}`, "", 0, doctor.Unverifiable, "github.push_unverifiable"},
		{"permissions request failed", "gh api --hostname github.com --method GET repos/octo/repo", "", "", 1, doctor.Unverifiable, "github.push_unverifiable"},
		{"invalid metadata", "gh api --hostname github.com --method GET repos/octo/repo", `null`, "", 0, doctor.Error, "github.invalid_response"},
		{"label request failed", "gh api --hostname github.com --method GET --paginate repos/octo/repo/labels?per_page=100", "", "", 1, doctor.Error, "github.labels_check_failed"},
		{"label request unreachable", "gh api --hostname github.com --method GET --paginate repos/octo/repo/labels?per_page=100", "", "error connecting to api.github.com", 1, doctor.Unverifiable, "github.labels_check_failed"},
		{"label request rate limited", "gh api --hostname github.com --method GET --paginate repos/octo/repo/labels?per_page=100", "", "API rate limit exceeded (HTTP 429)", 1, doctor.Unverifiable, "github.labels_check_failed"},
		{"invalid labels", "gh api --hostname github.com --method GET --paginate repos/octo/repo/labels?per_page=100", `{}`, "", 0, doctor.Error, "github.invalid_response"},
		{"tree unavailable", "git cat-file --batch-check=%(objecttype) --follow-symlinks", "", "", 1, doctor.Unverifiable, "git.tree_unverifiable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, fake, opts := repositorySetup(t, "")
			fake.responses[tc.command] = runner.ExecResult{Stdout: []byte(tc.output), Stderr: []byte(tc.stderr), ExitCode: tc.exit}
			requireFinding(t, doctor.Check(context.Background(), path, opts), tc.severity, tc.code, "octo/repo")
		})
	}
}

func TestUnreachableGitHubKeepsRepositoryChecksUnverifiable(t *testing.T) {
	path, fake, opts := repositorySetup(t, "")
	unreachable := runner.ExecResult{ExitCode: 1, Stderr: []byte("error connecting to api.github.com")}
	for _, command := range []string{
		"gh api --hostname github.com --method GET repos/octo/repo",
		"gh api --hostname github.com --method GET --paginate repos/octo/repo/labels?per_page=100",
	} {
		fake.responses[command] = unreachable
	}
	fake.responses["git ls-remote --symref -- https://github.com/octo/repo.git HEAD"] = runner.ExecResult{ExitCode: 128, Stderr: []byte("fatal: unable to access 'https://github.com/octo/repo.git/': Could not resolve host: github.com")}
	report := doctor.Check(context.Background(), path, opts)
	if report.HasErrors() {
		t.Fatalf("an unreachable GitHub must not fail doctor: %+v", report)
	}
	requireFinding(t, report, doctor.Unverifiable, "github.push_unverifiable", "octo/repo")
	requireFinding(t, report, doctor.Unverifiable, "github.labels_check_failed", "octo/repo")
	requireFinding(t, report, doctor.Unverifiable, "git.origin_inaccessible", "octo/repo")
}

// timingOutExecutor answers every command containing marker as if it hung
// until the check deadline.
type timingOutExecutor struct {
	*fakeCommands
	marker string
}

func (e timingOutExecutor) Exec(ctx context.Context, req runner.ExecRequest) (runner.ExecResult, error) {
	if strings.Contains(strings.Join(req.Args, " "), e.marker) {
		return runner.ExecResult{}, fmt.Errorf("executor: %w", context.DeadlineExceeded)
	}
	return e.fakeCommands.Exec(ctx, req)
}

func TestGitHubReadsThatOutrunTheDeadlineAreUnverifiable(t *testing.T) {
	path, fake, opts := repositorySetup(t, "")
	opts.Executor = timingOutExecutor{fakeCommands: fake, marker: "ls-remote"}
	report := doctor.Check(context.Background(), path, opts)
	if report.HasErrors() {
		t.Fatalf("a deadline on a GitHub read must not fail doctor: %+v", report)
	}
	requireFinding(t, report, doctor.Unverifiable, "git.origin_inaccessible", "octo/repo")
}

func TestBaseBranchReadsThatOutrunTheDeadlineAreUnverifiable(t *testing.T) {
	path, fake, opts := repositorySetup(t, "")
	opts.Executor = timingOutExecutor{fakeCommands: fake, marker: "ls-remote --exit-code"}
	report := doctor.Check(context.Background(), path, opts)
	if report.HasErrors() {
		t.Fatalf("a deadline on the base branch read must not fail doctor: %+v", report)
	}
	requireFinding(t, report, doctor.Unverifiable, "git.base_branch", "octo/repo")
}

func TestRepositoryLabelOverridesAcrossPages(t *testing.T) {
	path, fake, opts := repositorySetup(t, "")
	setConfig(t, path, "repositories.0.labels", map[string]string{"ready": "work", "running": "busy", "needs_attention": "help"})
	fake.responses["gh api --hostname github.com --method GET --paginate repos/octo/repo/labels?per_page=100"] = runner.ExecResult{Stdout: []byte(`[{"name":"WORK"}][{"name":"busy"},{"name":"help"}]`)}
	fake.repositoryFiles = fakeRegularRepositoryFiles("AGENTS.md\n")
	report := doctor.Check(context.Background(), path, opts)
	if len(report.Findings) != 0 {
		t.Fatalf("configured labels on separate pages: %+v", report)
	}
}

func TestClaudeInstructionWarningBelowMinimumVersion(t *testing.T) {
	path, fake, opts := repositorySetup(t, "")
	setConfig(t, path, "repositories.0.implementer.agent", "claude")
	fake.responses["claude --version"] = runner.ExecResult{Stdout: []byte("2.1.276 (Claude Code)")}
	fake.repositoryFiles = fakeRegularRepositoryFiles("AGENTS.md\n")
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
