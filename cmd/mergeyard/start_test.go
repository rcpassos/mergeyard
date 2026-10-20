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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/doctor"
	"github.com/rcpassos/mergeyard/internal/runtimeauth"
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

func TestRepositoryScopedDoctorErrorsDoNotStopStartup(t *testing.T) {
	cfg, _, err := config.Parse([]byte("repositories: [{repo: owner/broken}, {repo: owner/disabled, enabled: false}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	report := doctor.Report{Findings: []doctor.Finding{
		{Severity: doctor.Error, Code: "github.labels_check_failed", Scope: "owner/broken", Message: "repository is gone"},
		{Severity: doctor.Error, Code: "github.invalid_response", Scope: "owner/disabled", Message: "malformed GitHub response"},
		{Severity: doctor.Error, Code: "git.origin_inaccessible", Scope: "owner/broken implementer", Message: "repository is gone"},
	}}
	if blockingDoctorErrors(report, cfg) {
		t.Fatal("a repository access error refused startup")
	}
	report.Findings = append(report.Findings, doctor.Finding{Severity: doctor.Error, Code: "git.fetch", Scope: "owner/broken", Message: "local clone failed"})
	if !blockingDoctorErrors(report, cfg) {
		t.Fatal("a local repository failure did not refuse startup")
	}
	report.Findings = report.Findings[:len(report.Findings)-1]
	report.Findings = append(report.Findings, doctor.Finding{Severity: doctor.Error, Code: "internal.tool_missing", Scope: "gh", Message: "missing"})
	if !blockingDoctorErrors(report, cfg) {
		t.Fatal("a machine-scoped doctor error did not refuse startup")
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
	// The launcher is detached, so the runtime shutdown must leave it running.
	// The platform launcher records its arguments; the macOS AppleScript path
	// also records stdin because the tokenized URL travels there.
	browserStarted := filepath.Join(home, "browser-started")
	browserArgs := filepath.Join(home, "browser-args")
	browserStdin := filepath.Join(home, "browser-stdin")
	htmlHandler := filepath.Join(home, "html-handler")
	blockingScript := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + browserArgs + "'\nprintf '%s\\n' \"$$\" >> '" + browserStarted + "'\nexec /bin/sleep 60\n"
	osascriptScript := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + browserArgs + "'\n/bin/cat > '" + browserStdin + "'\nprintf '%s\\n' \"$$\" >> '" + browserStarted + "'\nexec /bin/sleep 60\n"
	htmlHandlerScript := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + htmlHandler + "'\n"
	for name, script := range map[string]string{
		"osascript":    osascriptScript,
		"gio":          blockingScript,
		"gtk-launch":   blockingScript,
		"xdg-open":     htmlHandlerScript,
		"xdg-settings": "#!/bin/sh\necho browser.desktop\n",
	} {
		if err := os.WriteFile(filepath.Join(tools, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	dataHome := filepath.Join(home, "data")
	desktop := filepath.Join(dataHome, "applications", "browser.desktop")
	if err := os.MkdirAll(filepath.Dir(desktop), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(desktop, []byte("[Desktop Entry]\n"), 0600); err != nil {
		t.Fatal(err)
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
	env := append(os.Environ(), "HOME="+home, "PATH="+tools, "XDG_DATA_HOME="+dataHome, "XDG_DATA_DIRS=")

	start := func(args ...string) (*exec.Cmd, string) {
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
			return cmd, line
		case <-ctx.Done():
			t.Fatal("CLI did not become ready")
		}
		return nil, ""
	}
	first, readyLine := start("start")
	root := filepath.Join(home, ".mergeyard")
	for _, name := range []string{"state.db", "mergeyard.lock", "runtime.secret", "repos", "worktrees", "runs"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("startup did not create %s: %v", name, err)
		}
	}
	info, err := os.Stat(filepath.Join(root, "runtime.secret"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("runtime secret permissions: %v %v", info, err)
	}
	firstSecret, err := runtimeauth.LoadSecret(root)
	if err != nil {
		t.Fatal(err)
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
	tokenAt := strings.Index(readyLine, "?token=")
	if tokenAt < 0 {
		t.Fatalf("startup link carried no dashboard token: %q", readyLine)
	}
	token := readyLine[tokenAt+len("?token="):]
	launcherArgs, err := os.ReadFile(browserArgs)
	if err != nil {
		t.Fatalf("browser launcher arguments: %v", err)
	}
	if strings.Contains(string(launcherArgs), token) || strings.Contains(string(launcherArgs), "token=") {
		t.Fatalf("browser launcher received the dashboard token: %q", launcherArgs)
	}
	urlAt := strings.Index(readyLine, "dashboard at ")
	if urlAt < 0 {
		t.Fatalf("startup link carried no dashboard URL: %q", readyLine)
	}
	dashboardURL := readyLine[urlAt+len("dashboard at "):]
	switch runtime.GOOS {
	case "darwin":
		if strings.TrimSpace(string(launcherArgs)) != "-" {
			t.Fatalf("macOS launcher arguments: %q", launcherArgs)
		}
		stdin, err := os.ReadFile(browserStdin)
		if err != nil || !strings.Contains(string(stdin), `open location "`+dashboardURL+`"`) {
			t.Fatalf("AppleScript did not open the default browser: %q %v", stdin, err)
		}
	case "linux":
		fields := strings.Split(strings.TrimSpace(string(launcherArgs)), "\n")
		if len(fields) != 3 || fields[0] != "launch" || fields[1] != desktop {
			t.Fatalf("Linux launcher did not select the default web browser: %q", launcherArgs)
		}
		pagePath := fields[2]
		assertHandoffPage(t, pagePath)
		relative, err := filepath.Rel(home, pagePath)
		if err != nil || strings.HasPrefix(relative, ".") || !strings.HasPrefix(relative, linuxDashboardLinksDirName+string(filepath.Separator)) {
			t.Fatalf("handoff page is not in a Snap-accessible home location: %q %v", relative, err)
		}
		page, err := os.ReadFile(pagePath)
		if err != nil || !strings.Contains(string(page), dashboardURL) {
			t.Fatalf("handoff page does not redirect to the link: %q %v", page, err)
		}
		if _, err := os.Stat(htmlHandler); !os.IsNotExist(err) {
			t.Fatalf("html file association was used instead of the web browser: %v", err)
		}
	default:
		assertHandoffPage(t, strings.TrimSpace(string(launcherArgs)))
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
	if runtime.GOOS == "linux" {
		// The browser the runtime launched is detached: shutting the runtime
		// down must not terminate it.
		for _, pid := range browserProcesses(t, browserStarted) {
			assertProcessAlive(t, pid)
			assertDetachedProcess(t, pid)
		}
	}
	// No arguments also dispatches start; the persistent lock file is reusable
	// and the restarted runtime rotates the secret it shares with the CLI.
	restarted, _ := start()
	rotated, err := runtimeauth.LoadSecret(root)
	if err != nil || string(rotated) == string(firstSecret) {
		t.Fatalf("restart did not rotate the runtime secret: %v", err)
	}
	if err := restarted.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Wait(); err != nil {
		t.Fatalf("SIGINT shutdown: %v", err)
	}
	// An ungraceful exit must also release the OS lock.
	killed, _ := start("start")
	if err := killed.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	killed.Wait()
	afterKill, _ := start("start")
	afterKill.Process.Signal(syscall.SIGTERM)
	if err := afterKill.Wait(); err != nil {
		t.Fatalf("restart after kill: %v", err)
	}
}
