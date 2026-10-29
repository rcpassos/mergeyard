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
	for _, input := range []string{"n\n", "\n", "", "y\n"} {
		t.Run(input, func(t *testing.T) {
			home, log := playgroundTools(t)
			var out bytes.Buffer
			path, _ := preparePlayground(context.Background(), strings.NewReader(input), &out)
			if path != "" {
				t.Fatalf("unexpected config: %s", path)
			}
			commands, err := os.ReadFile(log)
			if err != nil || string(commands) != "gh api --hostname github.com user\n" {
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
	path, err := preparePlayground(context.Background(), strings.NewReader("y\n\n\n"), &out)
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
	path, err := preparePlayground(context.Background(), strings.NewReader("y\n\n\n"), &out)
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
	path, err := preparePlayground(context.Background(), strings.NewReader("y\n\n\n"), &out)
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
