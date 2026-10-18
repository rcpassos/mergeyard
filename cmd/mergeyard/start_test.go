package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type safeBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

// startupToolScripts pass every dependency, authentication, and repository
// doctor check so startup reaches reconciliation. Scripts vary only in the
// GitHub issue listing and the tmux session inventory.
func startupToolScripts() map[string]string {
	return map[string]string{
		"git": `#!/bin/sh
case "$*" in
  *"ls-remote --symref"*) printf 'ref: refs/heads/main\tHEAD\n';;
  *"ls-remote --exit-code --heads"*) printf '0000000000000000000000000000000000000000\trefs/heads/main\n';;
  *"cat-file"*) printf 'refs/heads/doctor:CLAUDE.md missing\nrefs/heads/doctor:AGENTS.md missing\n';;
esac
exit 0
`,
		"tmux":   "#!/bin/sh\ncase \"$1\" in\n  -V) exit 0;;\nesac\necho 'no server running' >&2\nexit 1\n",
		"claude": "#!/bin/sh\ncase \"$1\" in\n  --version) echo '2.1.277 (Claude Code)';;\n  auth) exit 0;;\n  *) exit 1;;\nesac\n",
	}
}

func writeStartupTools(t *testing.T, dir string, scripts map[string]string) {
	t.Helper()
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// startupConfig selects the explicit workspace and port and keeps the poll
// interval short enough to observe later ticks during the test.
func startupConfig(workspace string, port int, disabled bool) string {
	return fmt.Sprintf("workspace: %q\nport: %d\npoll_interval: 50ms\nopen_browser: false\nagents: {claude: {permission_mode: acceptEdits}}\nrepositories:\n  - repo: owner/repo\n    enabled: %t\n    base_branch: main\n", workspace, port, !disabled)
}

func TestStartupServesDashboardAndRetriesWhenGitHubIsUnreachable(t *testing.T) {
	home, tools := t.TempDir(), t.TempDir()
	workspace, listings := filepath.Join(home, "workspace"), filepath.Join(home, "listings")
	scripts := startupToolScripts()
	// Dependency checks pass, but every GitHub read fails. Doctor reports the
	// repository and authentication checks as unverifiable, and reconcile fails
	// on the issue listing the way an outage or rate limit does.
	scripts["gh"] = `#!/bin/sh
case "$*" in
  *"auth status"*) echo 'Failed to log in to github.com account octo. The token in keyring is invalid.' >&2; exit 1;;
  *"rate_limit"*) echo 'Get "https://api.github.com/rate_limit": dial tcp: lookup api.github.com: no such host' >&2; exit 1;;
  *"repos/owner/repo/issues?state=open&per_page=100&labels="*) printf 'listing\n' >> '` + listings + `'; echo 'API rate limit exceeded (HTTP 429)' >&2; exit 1;;
  *"repos/owner/repo"*) echo 'error connecting to api.github.com' >&2; exit 1;;
esac
exit 0
`
	scripts["git"] = `#!/bin/sh
case "$*" in
  *"--version") exit 0;;
esac
echo "fatal: unable to access 'https://github.com/owner/repo.git/': Could not resolve host: github.com" >&2
exit 128
`
	writeStartupTools(t, tools, scripts)
	t.Setenv("HOME", home)
	t.Setenv("PATH", tools)
	port := freePort(t)
	path := filepath.Join(home, "config.yaml")
	// Only reconcile lists issues: the dashboard queue skips disabled
	// repositories, so every recorded listing belongs to a reconcile.
	data := startupConfig(workspace, port, true)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdout, stderr := &safeBuffer{}, &safeBuffer{}
	done := make(chan error, 1)
	go func() { done <- serve(ctx, path, stdout, stderr) }()
	address := fmt.Sprintf("http://127.0.0.1:%d/", port)
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(15 * time.Second)
	for {
		response, err := client.Get(address)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("dashboard did not serve while GitHub was unreachable: %v, stderr: %s", err, stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if strings.Contains(stderr.String(), "dependency checks failed") || !strings.Contains(stderr.String(), "unverifiable:") {
		t.Fatalf("unreachable GitHub still failed dependency checks: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "startup reconcile deferred to later ticks") || !strings.Contains(stderr.String(), "github.rate_limited") {
		t.Fatalf("startup did not report the deferred reconcile: %s", stderr.String())
	}
	// The running loop retries the failed reconcile on later ticks. The startup
	// reconcile wrote the first listing; a later tick must write another.
	deadline = time.Now().Add(10 * time.Second)
	for {
		content, _ := os.ReadFile(listings)
		if strings.Count(string(content), "listing") >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reconcile was not retried on a later tick: %q, stderr: %s", content, stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("shutdown: %v, stderr: %s", err, stderr.String())
	}
}

func TestStartupStopsWhenReconcileFailsOnLocalState(t *testing.T) {
	home, tools := t.TempDir(), t.TempDir()
	workspace := filepath.Join(home, "workspace")
	scripts := startupToolScripts()
	// GitHub answers; the tmux session inventory is local state that dispatch
	// depends on.
	scripts["gh"] = `#!/bin/sh
case "$*" in
  *"repos/owner/repo/labels?per_page=100"*) printf '%s\n' '[{"name":"ready-for-agent"},{"name":"agent-running"},{"name":"agent-needs-attention"}]';;
  *"repos/owner/repo/issues?state=open&per_page=100"*) printf '%s\n' '[]';;
  *"repos/owner/repo"*) printf '%s\n' '{"private":true,"permissions":{"push":true}}';;
esac
exit 0
`
	scripts["tmux"] = "#!/bin/sh\ncase \"$1\" in\n  -V) exit 0;;\nesac\necho 'tmux: server socket unavailable' >&2\nexit 1\n"
	writeStartupTools(t, tools, scripts)
	t.Setenv("HOME", home)
	t.Setenv("PATH", tools)
	port := freePort(t)
	path := filepath.Join(home, "config.yaml")
	data := startupConfig(workspace, port, false)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr := &safeBuffer{}, &safeBuffer{}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := serve(ctx, path, stdout, stderr)
	if err == nil || !strings.Contains(err.Error(), "reconcile existing runs") || !strings.Contains(err.Error(), "phase.tmux_failed") {
		t.Fatalf("startup continued on a local reconcile failure: %v, stderr: %s", err, stderr.String())
	}
	client := &http.Client{Timeout: time.Second}
	response, requestErr := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if requestErr == nil {
		response.Body.Close()
		t.Fatal("dashboard served after a local startup reconcile failure")
	}
}

func TestStartProcessesLockAndShutdown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "mergeyard")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	home := t.TempDir()
	tools := t.TempDir()
	for name, script := range map[string]string{
		"git": "#!/bin/sh\nexit 0\n", "tmux": "#!/bin/sh\nexit 0\n", "gh": "#!/bin/sh\nexit 0\n",
		"claude": "#!/bin/sh\ncase \"$1\" in --version) echo '2.1.277 (Claude Code)';; auth) exit 0;; *) exit 1;; esac\n",
	} {
		if err := os.WriteFile(filepath.Join(tools, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	// Keep the browser launcher blocked throughout startup and control requests.
	// SIGINT/SIGTERM must also terminate and reap it during runtime shutdown.
	browserStarted := filepath.Join(home, "browser-started")
	browserScript := "#!/bin/sh\nprintf '%s\\n' \"$$\" >> '" + browserStarted + "'\nexec /bin/sleep 60\n"
	for _, name := range []string{"open", "xdg-open"} {
		if err := os.WriteFile(filepath.Join(tools, name), []byte(browserScript), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		data, _ := os.ReadFile(browserStarted)
		for _, line := range strings.Fields(string(data)) {
			if pid, err := strconv.Atoi(line); err == nil {
				process, _ := os.FindProcess(pid)
				if process != nil {
					process.Kill()
				}
			}
		}
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	path := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("port: %d\nopen_browser: true\nagents: {claude: {permission_mode: acceptEdits}}\n", port)), 0600); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "HOME="+home, "PATH="+tools)

	start := func(args ...string) *exec.Cmd {
		t.Helper()
		cmd := exec.CommandContext(ctx, binary, append(args, "--config", path)...)
		cmd.Env = env
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		ready := make(chan string, 1)
		go func() {
			scanner := bufio.NewScanner(stdout)
			scanner.Scan()
			ready <- scanner.Text()
		}()
		select {
		case line := <-ready:
			if !strings.Contains(line, "workspace ready") {
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatalf("CLI not ready: %q, stderr: %s", line, &stderr)
			}
		case <-ctx.Done():
			t.Fatal("CLI did not become ready")
		}
		return cmd
	}
	first := start("start")
	root := filepath.Join(home, ".mergeyard")
	for _, name := range []string{"state.db", "mergeyard.lock", "repos", "worktrees", "runs"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("startup did not create %s: %v", name, err)
		}
	}
	second := exec.CommandContext(ctx, binary, "start", "--config", path)
	second.Env = env
	output, err := second.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() == 0 || !strings.Contains(string(output), "workspace.locked") {
		t.Fatalf("second process must fail with a lock error: %v, %s", err, output)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(browserStarted); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("browser launcher did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	browserClient := &http.Client{Timeout: 2 * time.Second}
	response, err := browserClient.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatalf("dashboard blocked by browser launch: %v", err)
	}
	response.Body.Close()
	browserClient.CloseIdleConnections()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status: %d", response.StatusCode)
	}
	for _, action := range []string{"status", "pause", "status", "resume"} {
		controlCtx, stopControl := context.WithTimeout(ctx, 2*time.Second)
		cmd := exec.CommandContext(controlCtx, binary, action, "--config", path)
		cmd.Env = env
		output, err := cmd.CombinedOutput()
		stopControl()
		if err != nil {
			t.Fatalf("%s: %v %s", action, err, output)
		}
		if action == "pause" && !strings.Contains(string(output), "paused") {
			t.Fatalf("pause: %s", output)
		}
	}
	if err := first.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := first.Wait(); err != nil {
		t.Fatalf("SIGTERM shutdown: %v", err)
	}
	// No arguments also dispatches start; the persistent lock file is reusable.
	restarted := start()
	if err := restarted.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Wait(); err != nil {
		t.Fatalf("SIGINT shutdown: %v", err)
	}
	// An ungraceful exit must also release the OS lock.
	killed := start("start")
	if err := killed.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	killed.Wait()
	afterKill := start("start")
	afterKill.Process.Signal(syscall.SIGTERM)
	if err := afterKill.Wait(); err != nil {
		t.Fatalf("restart after kill: %v", err)
	}
}
