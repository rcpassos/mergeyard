package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/doctor"
	"github.com/rcpassos/mergeyard/internal/harness/registry"
	"github.com/rcpassos/mergeyard/internal/repository"
)

// The template is embedded here so trying Mergeyard needs no separately
// maintained template repository, language runtime, or dependency downloads.
var playgroundFiles = map[string]string{
	"README.md": `# Mergeyard playground

A disposable project for watching Mergeyard turn an issue into a reviewed PR.
Run the greeting with ` + "`sh greet.sh`" + ` and the tests with ` + "`sh test.sh`" + `.
The project uses only a POSIX shell. Keep changes small and run the tests before
committing. Review the PR in the dashboard and decide whether to merge it.
`,
	"greet.sh": "#!/bin/sh\nprintf 'Hello, world!\\n'\n",
	"test.sh": `#!/bin/sh
set -eu
cd "$(dirname "$0")"
actual=$(sh greet.sh)
if [ "$actual" != 'Hello, world!' ]; then
    printf 'Expected Hello, world!; got: %s\n' "$actual" >&2
    exit 1
fi
printf 'All tests passed.\n'
`,
	".github/workflows/test.yml": `name: Tests
on: [push, pull_request]
permissions:
  contents: read
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262 # v4.4.0
        with:
          persist-credentials: false
      - run: sh test.sh
`,
}

const playgroundIssue = `## Goal

Let the greeting accept an optional name as its first argument.

## Acceptance criteria

- ` + "`sh greet.sh`" + ` still prints exactly "Hello, world!" followed by a newline.
- ` + "`sh greet.sh Ada`" + ` prints "Hello, Ada!" followed by a newline.
- ` + "`sh greet.sh 'Ada Lovelace'`" + ` preserves the space in the name.
- An explicitly empty name behaves like no argument.
- Names containing percent signs or shell metacharacters are printed literally;
  never evaluate the name as code or use it as a printf format string.
- Extend ` + "`test.sh`" + ` to cover these cases. ` + "`sh test.sh`" + ` must pass.
- Use POSIX shell only, with no new dependencies. Keep the existing CI workflow.

This is a disposable Mergeyard demonstration. Open a PR; the user decides what merges.
`

func tryPlayground(stdout, stderr io.Writer) int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	path, err := preparePlayground(ctx, os.Stdin, stdout, doctor.Options{})
	if err != nil {
		fmt.Fprintf(stderr, "mergeyard try: %v\n", err)
		return 1
	}
	if path == "" {
		return 0
	}
	if code := checkSetup(ctx, path, stdout, stderr); code != 0 {
		return code
	}
	if err := serve(ctx, path, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "mergeyard try: %v\n", err)
		return 1
	}
	return 0
}

// preparePlayground never loads the user's normal config and only mutates the
// newly named repository after confirmation. Every invocation creates a fresh
// playground so an existing repository can never be mistaken for disposable data.
func preparePlayground(ctx context.Context, input io.Reader, output io.Writer, doctorOptions doctor.Options) (string, error) {
	var agents []string
	for _, name := range []string{"git", "gh", "tmux"} {
		if _, err := exec.LookPath(name); err != nil {
			return "", fmt.Errorf("install %s before running mergeyard try", name)
		}
	}
	for _, name := range registry.Names() {
		if _, err := exec.LookPath(name); err == nil {
			agents = append(agents, name)
		}
	}
	if len(agents) == 0 {
		return "", errors.New("install and authenticate Claude Code or Codex before running mergeyard try")
	}
	data, err := setupGH(ctx, "api", "--hostname", "github.com", "user")
	if err != nil {
		return "", err
	}
	var account struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(data, &account); err != nil || !repository.ValidName(account.Login+"/playground") {
		return "", errors.New("GitHub returned an invalid account; check gh auth status")
	}
	repo := account.Login + "/mergeyard-playground-" + uuid.NewString()[:8]
	prompts := setupPrompts{ctx, bufio.NewReader(input), output}
	implementer, err := prompts.agent("Implementer", agents)
	if err != nil {
		return "", err
	}
	reviewer, err := prompts.agent("Reviewer", agents)
	if err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	root := filepath.Join(home, ".local", "share", "mergeyard", "playgrounds", strings.TrimPrefix(repo, account.Login+"/"))
	cfg, doc, err := config.Parse([]byte("{}"))
	if err != nil {
		return "", err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return "", err
	}
	if err := doc.SetRoleAgents(implementer, reviewer); err != nil {
		return "", err
	}
	for _, setting := range []struct {
		key   string
		value any
	}{
		{"workspace", filepath.Join(root, "workspace")},
		{"port", port},
		{"poll_interval", "5s"},
		{"trusted_labelers", []string{account.Login}},
	} {
		if err := doc.Set([]string{setting.key}, setting.value); err != nil {
			return "", err
		}
	}
	// No repositories yet: doctor checks the machine and the selected global
	// roles without probing a remote repository that does not exist.
	if err := playgroundPreflight(ctx, doc, output, doctorOptions); err != nil {
		return "", err
	}
	fmt.Fprintf(output, "Create private GitHub repository %s with a small shell project, tests, CI, queue labels, and one sample issue.\nAgents use Mergeyard defaults: Claude bypassPermissions; Codex workspace-write with network access.\nThis uses your agent allowance and GitHub Actions allowance.\nA separate config and workspace will be used. You decide whether to merge the resulting PR.\n", repo)
	confirmed, err := prompts.confirm("Create this playground and start its agents?")
	if err != nil {
		return "", err
	}
	if !confirmed {
		fmt.Fprintln(output, "Playground cancelled.")
		return "", nil
	}
	if err := os.MkdirAll(filepath.Dir(root), 0700); err != nil {
		return "", err
	}
	if err := os.Mkdir(root, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(root, "config.yaml")
	// Print recovery and cleanup before the first remote write, including when
	// repo creation succeeds but a push or later setup step fails.
	fmt.Fprintf(output, "Playground directory: %s\nRepository: https://github.com/%s\nAfter setup, resume with: mergeyard --config %s start\nBefore cleanup, stop the sample run in the dashboard and resolve any reported leftovers.\nCtrl-C stops the dashboard but leaves running agent sessions alive.\nThen stop Mergeyard and review anything you want to keep before running:\n  gh repo delete %s --yes\n  rm -rf -- %s\n", root, repo, shellQuote(path), shellQuote("github.com/"+repo), shellQuote(root))
	source := filepath.Join(root, "source")
	for name, content := range playgroundFiles {
		destination := filepath.Join(source, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
			return "", err
		}
		if err := os.WriteFile(destination, []byte(content), 0600); err != nil {
			return "", err
		}
	}
	for _, args := range [][]string{
		{"init", "--initial-branch=main"},
		{"add", "."},
		{"-c", "user.name=Mergeyard Playground", "-c", "user.email=playground@mergeyard.invalid", "-c", "commit.gpgsign=false", "commit", "-m", "Initialize Mergeyard playground"},
	} {
		if err := playgroundGit(ctx, source, args...); err != nil {
			return "", err
		}
	}
	if _, err := setupGH(ctx, "repo", "create", "github.com/"+repo, "--private", "--disable-wiki", "--description", "Disposable Mergeyard playground", "--source", source, "--push"); err != nil {
		return "", fmt.Errorf("create/push playground (check Git credentials and permission to push workflow files): %w", err)
	}
	if err := createSetupLabels(ctx, repo, cfg.Labels); err != nil {
		return "", err
	}
	if err := doc.Set([]string{"repositories"}, []map[string]any{{"repo": repo, "base_branch": "main", "checks": []string{"sh test.sh"}}}); err != nil {
		return "", err
	}
	if err := doc.Write(path); err != nil {
		return "", err
	}
	issue, err := setupGH(ctx, "issue", "create", "--repo", "github.com/"+repo, "--title", "Greet a person by name", "--body", playgroundIssue, "--label", cfg.Labels.Ready)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(output, "Sample issue: %s\nRunning doctor, then starting the playground dashboard. Press Ctrl-C to stop.\n", strings.TrimSpace(string(issue)))
	return path, nil
}

func playgroundPreflight(ctx context.Context, doc *config.Document, output io.Writer, options doctor.Options) error {
	dir, err := os.MkdirTemp("", "mergeyard-playground-preflight-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "config.yaml")
	if err := doc.Write(path); err != nil {
		return err
	}
	fmt.Fprintln(output, "Checking machine and selected agents before creating a playground.")
	report := doctor.Check(ctx, path, options)
	if err := report.Write(output); err != nil {
		return err
	}
	if report.HasErrors() {
		return errors.New("prerequisite checks failed; fix doctor findings before creating a playground")
	}
	return ctx.Err()
}

func playgroundGit(ctx context.Context, dir string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("initialize playground Git repository: %w", err)
	}
	return nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
