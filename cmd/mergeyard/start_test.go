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
	"syscall"
	"testing"
	"time"
)

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
