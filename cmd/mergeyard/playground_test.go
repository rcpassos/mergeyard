package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/doctor"
)

func playgroundTools(t *testing.T) (string, string) {
	t.Helper()
	home, log := setupTools(t)
	tools := os.Getenv("PATH")
	for name, script := range map[string]string{
		"git": `#!/bin/sh
printf 'git %s\n' "$*" >> "$SETUP_LOG"
`,
		"gh": `#!/bin/sh
printf 'gh %s\n' "$*" >> "$SETUP_LOG"
case "$*" in
 *"$PLAYGROUND_FAIL"*) if [ -n "$PLAYGROUND_FAIL" ]; then exit 1; fi;;
esac
case "$*" in
 'api --hostname github.com user') echo '{"login":"octo"}';;
 *'/labels?'*) echo '[]';;
 'issue create '*) echo 'https://github.com/octo/playground/issues/1';;
esac
`,
	} {
		if err := os.WriteFile(filepath.Join(tools, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return home, log
}

func TestPlaygroundRequiresConsentBeforeAnyMutations(t *testing.T) {
	for _, input := range []string{"\n\nn\n", "\n\n\n", "\n\n", "\n", ""} {
		t.Run(input, func(t *testing.T) {
			home, log := playgroundTools(t)
			var out bytes.Buffer
			path, _ := preparePlayground(context.Background(), strings.NewReader(input), &out, doctor.Options{EffectiveUID: func() int { return 1000 }})
			if path != "" {
				t.Fatalf("unexpected config: %s", path)
			}
			commands, err := os.ReadFile(log)
			if err != nil || strings.Contains(string(commands), "gh repo create") || strings.Contains(string(commands), "gh label create") || strings.Contains(string(commands), "gh issue create") || strings.Contains(string(commands), "init --initial-branch") {
				t.Fatalf("mutated without completed confirmation/agent choices: %s (%v)", commands, err)
			}
			if _, err := os.Stat(filepath.Join(home, ".local")); !os.IsNotExist(err) {
				t.Fatalf("created playground before completed prompts: %v", err)
			}
		})
	}
}

func TestPlaygroundCreatesIsolatedConfigAndOneReadyIssue(t *testing.T) {
	home, log := playgroundTools(t)
	t.Chdir(t.TempDir())
	original := "# existing configuration must stay untouched\nrepositories: [{repo: octo/real}]\n"
	for _, path := range []string{"mergeyard.yaml", filepath.Join(home, ".config", "mergeyard", "config.yaml")} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(original), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	path, err := preparePlayground(context.Background(), strings.NewReader("\n\ny\n"), &out, doctor.Options{EffectiveUID: func() int { return 1000 }})
	if err != nil {
		t.Fatalf("prepare: %v\n%s", err, &out)
	}
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(path)
	if !strings.HasPrefix(root, filepath.Join(home, ".local", "share", "mergeyard", "playgrounds")+string(os.PathSeparator)) || cfg.Workspace != filepath.Join(root, "workspace") || cfg.Port == 7331 || cfg.PollInterval.String() != "5s" {
		t.Fatalf("playground isolation: path=%s config=%+v", path, cfg)
	}
	if len(cfg.Repositories) != 1 || !strings.HasPrefix(cfg.Repositories[0].Repo, "octo/mergeyard-playground-") || cfg.Repositories[0].BaseBranch != "main" || strings.Join(cfg.Repositories[0].Checks, ",") != "sh test.sh" || len(cfg.TrustedLabelers) != 1 || cfg.TrustedLabelers[0] != "octo" {
		t.Fatalf("playground repository settings: %+v", cfg)
	}
	for _, path := range []string{"mergeyard.yaml", filepath.Join(home, ".config", "mergeyard", "config.yaml")} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != original {
			t.Fatalf("changed existing config %s: %s (%v)", path, data, err)
		}
	}
	commands, _ := os.ReadFile(log)
	for _, want := range []string{"--private", "--push", "label create", "-- ready-for-agent", "-- agent-running", "-- agent-needs-attention", "--label ready-for-agent", "init --initial-branch=main"} {
		if !strings.Contains(string(commands), want) {
			t.Errorf("missing %s in commands: %s", want, commands)
		}
	}
	if strings.Count(string(commands), "gh issue create") != 1 {
		t.Fatalf("expected one sample issue: %s", commands)
	}
	for _, want := range []string{"gh repo delete", "rm -rf --", "mergeyard --config", "private GitHub repository", "Ctrl-C"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing instructions %q: %s", want, &out)
		}
	}
	for name := range playgroundFiles {
		if _, err := os.Stat(filepath.Join(root, "source", filepath.FromSlash(name))); err != nil {
			t.Fatalf("missing template file %s: %v", name, err)
		}
	}
}

func TestPlaygroundFailureKeepsRecoveryInstructionsAndDoesNotQueueIssue(t *testing.T) {
	_, log := playgroundTools(t)
	t.Setenv("PLAYGROUND_FAIL", "repo create")
	var out bytes.Buffer
	path, err := preparePlayground(context.Background(), strings.NewReader("\n\ny\n"), &out, doctor.Options{EffectiveUID: func() int { return 1000 }})
	if err == nil || path != "" || !strings.Contains(out.String(), "gh repo delete") || !strings.Contains(out.String(), "rm -rf --") {
		t.Fatalf("failure recovery: path=%s err=%v output=%s", path, err, &out)
	}
	commands, _ := os.ReadFile(log)
	if strings.Contains(string(commands), "label create") || strings.Contains(string(commands), "issue create") {
		t.Fatalf("continued after creation failed: %s", commands)
	}
}

func TestPlaygroundRejectsNormalConfig(t *testing.T) {
	code, _, errOut := runCLI("try", "--config", "/unused/real-config.yaml")
	if code != 2 || !strings.Contains(errOut, "uses its own config") {
		t.Fatalf("config accepted: code=%d stderr=%s", code, errOut)
	}
}

func TestPlaygroundTemplateTestsRunAndDetectRegression(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"greet.sh", "test.sh"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(playgroundFiles[name]), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run := func() error {
		cmd := exec.Command("/bin/sh", "test.sh")
		cmd.Dir = dir
		return cmd.Run()
	}
	if err := run(); err != nil {
		t.Fatalf("fresh template fails: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "greet.sh"), []byte("printf 'wrong greeting\\n'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(); err == nil {
		t.Fatal("template tests did not detect broken greeting")
	}
}

func TestPlaygroundCommitsTemplateInNewGitRepository(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	playgroundTools(t)
	tools := os.Getenv("PATH")
	if err := os.Remove(filepath.Join(tools, "git")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(git, filepath.Join(tools, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	var out bytes.Buffer
	path, err := preparePlayground(context.Background(), strings.NewReader("\n\ny\n"), &out, doctor.Options{EffectiveUID: func() int { return 1000 }})
	if err != nil {
		t.Fatalf("prepare with real git: %v\n%s", err, &out)
	}
	source := filepath.Join(filepath.Dir(path), "source")
	cmd := exec.Command(git, "-C", source, "show", "main:test.sh")
	data, err := cmd.Output()
	if err != nil || string(data) != playgroundFiles["test.sh"] {
		t.Fatalf("template not committed to main: %s (%v)", data, err)
	}
	cmd = exec.Command(git, "-C", source, "status", "--porcelain")
	data, err = cmd.Output()
	if err != nil || len(data) != 0 {
		t.Fatalf("source worktree dirty: %s (%v)", data, err)
	}
}

func TestPlaygroundRejectsAgentProblemsBeforeCreation(t *testing.T) {
	for _, tc := range []struct {
		name, agent, script, code string
	}{
		{"claude not logged in", "claude", "case \"$1\" in --version) echo '2.1.277';; auth) exit 1;; esac", "harness.not_logged_in"},
		{"claude too old", "claude", "case \"$1\" in --version) echo '2.1.0';; esac", "harness.version_unsupported"},
		{"codex not logged in", "codex", "case \"$1\" in --version) echo 'codex-cli 0.156.1';; login) exit 1;; esac", "harness.not_logged_in"},
		{"codex too old", "codex", "case \"$1\" in --version) echo 'codex-cli 0.100.0';; esac", "harness.version_unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, log := playgroundTools(t)
			temporary := t.TempDir()
			t.Setenv("TMPDIR", temporary)
			if err := os.WriteFile(filepath.Join(os.Getenv("PATH"), tc.agent), []byte("#!/bin/sh\n"+tc.script+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			input := tc.agent + "\n" + tc.agent + "\ny\n"
			var out bytes.Buffer
			path, err := preparePlayground(context.Background(), strings.NewReader(input), &out, doctor.Options{EffectiveUID: func() int { return 1000 }})
			if err == nil || path != "" || !strings.Contains(out.String(), tc.code) {
				t.Errorf("agent problem did not fail preflight: path=%s err=%v output=%s", path, err, &out)
			}
			commands, _ := os.ReadFile(log)
			for _, mutation := range []string{"gh repo create", "gh label create", "gh issue create", "init --initial-branch"} {
				if strings.Contains(string(commands), mutation) {
					t.Errorf("agent problem left playground artifacts: %s", commands)
					break
				}
			}
			if _, err := os.Stat(filepath.Join(home, ".local")); !os.IsNotExist(err) {
				t.Errorf("agent problem left a playground directory: %v", err)
			}
			if strings.Contains(out.String(), "Create this playground") {
				t.Error("creation was offered before agent preflight passed")
			}
			if files, err := os.ReadDir(temporary); err != nil || len(files) != 0 {
				t.Errorf("preflight temporary files were not removed: %v (%v)", files, err)
			}
		})
	}
}

func TestPlaygroundPreflightUsesSelectedRolesForRootCheck(t *testing.T) {
	for _, tc := range []struct{ implementer, reviewer string }{
		{"claude", "codex"},
		{"codex", "claude"},
		{"codex", "codex"},
	} {
		t.Run(tc.implementer+"/"+tc.reviewer, func(t *testing.T) {
			home, log := playgroundTools(t)
			if err := os.WriteFile(filepath.Join(os.Getenv("PATH"), "codex"), []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'codex-cli 0.156.1'; fi\n"), 0700); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			input := tc.implementer + "\n" + tc.reviewer + "\ny\n"
			path, err := preparePlayground(context.Background(), strings.NewReader(input), &out, doctor.Options{EffectiveUID: func() int { return 0 }})
			if tc.implementer == "codex" && tc.reviewer == "codex" {
				if err != nil || path == "" {
					t.Fatalf("unselected Claude blocked Codex: %v\n%s", err, &out)
				}
				return
			}
			if err == nil || path != "" || !strings.Contains(out.String(), "harness.root_bypass_permissions") {
				t.Fatalf("root check did not stop setup: path=%s err=%v output=%s", path, err, &out)
			}
			commands, _ := os.ReadFile(log)
			if strings.Contains(string(commands), "gh repo create") || strings.Contains(out.String(), "Create this playground") {
				t.Fatalf("root check ran after creation was offered: %s\n%s", commands, &out)
			}
			if _, err := os.Stat(filepath.Join(home, ".local")); !os.IsNotExist(err) {
				t.Fatalf("root check left a playground directory: %v", err)
			}
		})
	}
}
