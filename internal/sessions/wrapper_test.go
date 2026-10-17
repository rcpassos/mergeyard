package sessions

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWrapperRejectsEqualsInExecutable(t *testing.T) {
	dir := t.TempDir()
	_, err := wrapper(Command{Executable: filepath.Join(dir, "agent=alias"), Dir: dir}, dir)
	if err == nil {
		t.Fatal("env would interpret executable as an assignment; want a launch error")
	}
}

func TestWrapperDoesNotPublishFailedMetadataWrite(t *testing.T) {
	dir := t.TempDir()
	script, err := wrapper(Command{Executable: "/bin/sh", Args: []string{"-c", "exit 0"}, Dir: dir}, dir)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a partial exit-metadata write after the process journal succeeds.
	script = strings.Replace(script, "#!/bin/sh\n", "#!/bin/sh\nprintf() { case \"$1\" in *exit_code*) /bin/echo '{'; return 1;; *) command printf \"$@\";; esac; }\n", 1)
	path := filepath.Join(dir, "wrapper.sh")
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("/bin/sh", path).CombinedOutput(); err != nil {
		t.Fatalf("wrapper: %v: %s", err, out)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "exit.json")); !os.IsNotExist(err) {
		t.Fatalf("failed write was published as metadata: %q, err=%v", data, err)
	}
}

func TestQuoteRoundTripsLiteralShellArguments(t *testing.T) {
	values := []string{"", "plain", "a b", "'\"; $HOME $(echo injected) `echo injected`", "line one\nline two", "-leading-dash", "界"}
	for _, value := range values {
		out, err := exec.Command("/bin/sh", "-c", "printf '%s' "+quote(value)).CombinedOutput()
		if err != nil {
			t.Fatalf("quote %q: %v: %s", value, err, out)
		}
		if string(out) != value {
			t.Fatalf("quote %q produced %q", value, out)
		}
	}
}

func TestWrapperValidationDoesNotRequireTmux(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cases := []Command{
		{Executable: "/bin/sh", Env: map[string]string{"": "value"}},
		{Executable: "/bin/sh", Env: map[string]string{"BAD=KEY": "value"}},
		{Executable: "/bin/sh", Env: map[string]string{"1BAD": "value"}},
		{Executable: "/bin/sh", Env: map[string]string{"VALID": "value\x00tail"}},
		{Executable: "/bin/sh", Args: []string{"arg\x00tail"}},
		{Executable: "/bin/sh", StdinPath: "file\x00tail"},
	}
	for _, command := range cases {
		_, err := wrapper(command, t.TempDir())
		assertCode(t, err, "phase.invalid_request")
	}
}

func TestGeneratedWrapperLogsAndMetadataWithoutTmux(t *testing.T) {
	dir := t.TempDir()
	phaseDir := filepath.Join(dir, "logs '$ ;")
	if err := os.Mkdir(phaseDir, 0700); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(dir, "input '$ ;")
	if err := os.WriteFile(input, []byte("input data\n"), 0600); err != nil {
		t.Fatal(err)
	}
	value := "literal ' ; $(echo injected)\nsecond line"
	command := Command{
		Executable: "/bin/sh", Args: []string{"-c", `printf '%s\n' "$VALUE" "$1"; /bin/cat; printf 'diagnostic' >&2; exit 7`, "fixture", value},
		Dir: dir, Env: map[string]string{"VALUE": value}, StdinPath: input,
	}
	script, err := wrapper(command, phaseDir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "wrapper.sh")
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("/bin/sh", append([]string{path}, command.Environment()...)...).Run(); err == nil {
		t.Fatal("wrapper discarded exit 7")
	} else {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 7 {
			t.Fatalf("wrapper exit: %v", err)
		}
	}
	out, err := os.ReadFile(filepath.Join(phaseDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != value+"\n"+value+"\ninput data\n" {
		t.Fatalf("stdout = %q", out)
	}
	stderr, err := os.ReadFile(filepath.Join(phaseDir, "stderr.log"))
	if err != nil || string(stderr) != "diagnostic" {
		t.Fatalf("stderr = %q, err=%v", stderr, err)
	}
	status, err := New(Options{}).Status(context.Background(), Ref{Name: "not-needed", PhaseDir: phaseDir})
	if err != nil || status.State != Exited || status.ExitCode == nil || *status.ExitCode != 7 {
		t.Fatalf("metadata = %+v, err=%v", status, err)
	}
}

func TestGeneratedWrapperCompletesDuringWatchdogStartup(t *testing.T) {
	dir := t.TempDir()
	starting := filepath.Join(dir, "watchdog-starting")
	signaled := filepath.Join(dir, "watchdog-signaled")
	script, err := wrapperTimeout(Command{
		Executable: "/bin/sh", Args: []string{"-c", "while [ ! -f " + quote(starting) + " ]; do /bin/sleep 0.01; done; exit 7"}, Dir: dir,
	}, dir, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// Hold the watchdog in startup with the parent's no-op signal handler,
	// before it installs the handler that terminates its sleep. The foreground
	// completes during that window, so a TERM can be consumed without stopping
	// the watchdog. Normal completion must still retire the full process group.
	// Release startup only after the signal is consumed, rather than relying on
	// a delay to make the foreground finish before the cleanup handler is ready.
	startup := " sleeper=\n trap " + quote(": >"+quote(signaled)) + " INT TERM HUP\n : >" + quote(starting) + "\n while [ ! -f " + quote(signaled) + " ]; do /bin/sleep 0.01; done\n"
	script = strings.Replace(script, " sleeper=\n", startup, 1)
	path := filepath.Join(dir, "wrapper.sh")
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("completion waited for the starting watchdog: %v: %s", err, out)
	}
	// A bounded wrapper kills its own group after publishing the command result.
	status, err := New(Options{}).Status(context.Background(), Ref{Name: "not-needed", PhaseDir: dir})
	if err != nil || status.State != Exited || status.ExitCode == nil || *status.ExitCode != 7 || status.TimedOut {
		t.Fatalf("completion metadata=%+v, err=%v", status, err)
	}
}

func TestManagerObservesWrapperLaunchedBeforeEnvironmentUpgrade(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is required for session integration tests")
	}
	dir := t.TempDir()
	options := Options{SocketName: "my-upgrade-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)}
	t.Cleanup(func() { exec.Command("tmux", "-L", options.SocketName, "kill-server").Run() })
	req := Request{RunID: "old-run", Phase: "implement", Attempt: 1, PhaseDir: dir, Command: Command{
		Executable: "/bin/sh", Dir: dir, Env: map[string]string{"OLD_VALUE": "retained"},
		Args: []string{"-c", `printf ready; while [ ! -f release ]; do /bin/sleep 0.02; done; [ "$OLD_VALUE" = retained ]`},
	}}
	script, err := wrapper(req.Command, dir)
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce the old on-disk launch contract: assignments in the wrapper,
	// no environment arguments supplied when tmux starts it.
	script = strings.Replace(script, `/usr/bin/env -i "$@"`, `/usr/bin/env -i 'OLD_VALUE=retained'`, 1)
	path := filepath.Join(dir, "wrapper.sh")
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ref := Ref{Name: Name(req), PhaseDir: dir}
	if out, err := exec.Command("tmux", "-L", options.SocketName, "-f", "/dev/null", "new-session", "-d", "-s", ref.Name, "--", "/bin/sh", path).CombinedOutput(); err != nil {
		t.Fatalf("start old wrapper: %v: %s", err, out)
	}
	manager := New(options)
	t.Cleanup(func() { manager.Stop(context.Background(), ref) })
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
		if err == nil && string(data) == "ready" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("old wrapper did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	status, err := manager.Status(context.Background(), ref)
	if err != nil || status.State != Running {
		t.Fatalf("old running session: %+v, err=%v", status, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for time.Now().Before(deadline) {
		status, err = manager.Status(context.Background(), ref)
		if err != nil {
			t.Fatal(err)
		}
		if status.State == Exited {
			if status.ExitCode == nil || *status.ExitCode != 0 {
				t.Fatalf("old wrapper lost environment: %+v", status)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("old wrapper did not complete: %+v", status)
}
