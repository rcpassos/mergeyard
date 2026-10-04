package runner_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/runner"
)

func localSessions(t *testing.T) (*runner.Local, runner.Options) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is required for session integration tests")
	}
	options := runner.Options{SocketName: fmt.Sprintf("my-test-%d-%d", os.Getpid(), time.Now().UnixNano()), GracePeriod: 200 * time.Millisecond}
	t.Cleanup(func() { exec.Command("tmux", "-L", options.SocketName, "kill-server").Run() })
	return runner.NewLocal(options), options
}

func phaseRequest(t *testing.T, script string) runner.SessionRequest {
	t.Helper()
	dir := t.TempDir()
	executable := filepath.Join(dir, "fake agent '$ ;.sh")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return runner.SessionRequest{RunID: "run-123456789", Phase: "implement", Round: 1, Attempt: 1,
		PhaseDir: filepath.Join(dir, "logs '$ ;"), Command: runner.ExecRequest{Executable: executable, Dir: dir}}
}

func awaitExit(t *testing.T, r runner.Runner, ref runner.SessionRef) runner.SessionStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, err := r.SessionStatus(context.Background(), ref)
		if err != nil {
			t.Fatal(err)
		}
		if status.State == runner.SessionExited {
			return status
		}
		time.Sleep(10 * time.Millisecond)
	}
	status, _ := r.SessionStatus(context.Background(), ref)
	stderr, _ := r.ReadFile(context.Background(), filepath.Join(ref.PhaseDir, "stderr.log"))
	script, _ := r.ReadFile(context.Background(), filepath.Join(ref.PhaseDir, "wrapper.sh"))
	t.Fatalf("phase did not exit: %+v, stderr: %s, wrapper: %s", status, stderr, script)
	return runner.SessionStatus{}
}

func TestSessionWritesLogsAndNonzeroExit(t *testing.T) {
	r, _ := localSessions(t)
	req := phaseRequest(t, "printf '%s\\n' \"$PWD\" \"$VALUE\" \"$1\"; cat; printf 'diagnostic' >&2; exit 7")
	req.Command.Env = map[string]string{"VALUE": "explicit value"}
	req.Command.Args = []string{"literal ' ; $(touch injected)"}
	req.Command.StdinPath = filepath.Join(req.Command.Dir, "input '$")
	if err := os.WriteFile(req.Command.StdinPath, []byte("input data\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ref, err := r.StartSession(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	status := awaitExit(t, r, ref)
	if status.ExitCode == nil || *status.ExitCode != 7 {
		t.Fatalf("status = %+v", status)
	}
	out, err := r.ReadFile(context.Background(), filepath.Join(ref.PhaseDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	worktree, err := filepath.EvalSymlinks(req.Command.Dir)
	if err != nil {
		t.Fatal(err)
	}
	want := worktree + "\nexplicit value\nliteral ' ; $(touch injected)\ninput data\n"
	if string(out) != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
	stderr, err := r.ReadFile(context.Background(), filepath.Join(ref.PhaseDir, "stderr.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stderr) != "diagnostic" {
		t.Fatalf("stderr = %q", stderr)
	}
	if _, err := os.Stat(filepath.Join(req.Command.Dir, "injected")); !os.IsNotExist(err) {
		t.Fatalf("argument was executed: %v", err)
	}
	if !strings.HasPrefix(ref.Name, "mergeyard-") {
		t.Fatalf("session name = %q", ref.Name)
	}
}

func TestSessionSurvivesManagerRestartAndContextCancellation(t *testing.T) {
	r, options := localSessions(t)
	req := phaseRequest(t, "printf 'ready\\n'; while [ ! -f release ]; do sleep 0.02; done; exit 0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ref, err := r.StartSession(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	restarted := runner.NewLocal(options)
	status, err := restarted.SessionStatus(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != runner.SessionRunning {
		t.Fatalf("status after restart = %+v", status)
	}
	if err := restarted.WriteFile(context.Background(), filepath.Join(req.Command.Dir, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	status = awaitExit(t, restarted, ref)
	if status.ExitCode == nil || *status.ExitCode != 0 {
		t.Fatalf("status = %+v", status)
	}
}

func TestSessionUsesOnlyExplicitEnvironmentAndClosedStdin(t *testing.T) {
	r, _ := localSessions(t)
	req := phaseRequest(t, "read ignored && exit 9; exec /usr/bin/env")
	// env itself avoids shell-added variables when checking environment isolation.
	req.Command.Executable = "/usr/bin/env"
	req.Command.Env = map[string]string{"ONLY": "configured"}
	ref, err := r.StartSession(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	status := awaitExit(t, r, ref)
	if status.ExitCode == nil || *status.ExitCode != 0 {
		t.Fatalf("status = %+v", status)
	}
	out, err := r.ReadFile(context.Background(), filepath.Join(ref.PhaseDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "ONLY=configured\n" {
		t.Fatalf("environment = %q", out)
	}
	// A second phase verifies that omitted stdin is EOF rather than a tmux TTY.
	req = phaseRequest(t, "read ignored && exit 9; exit 0")
	req.Attempt = 2
	ref, err = r.StartSession(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	status = awaitExit(t, r, ref)
	if status.ExitCode == nil || *status.ExitCode != 0 {
		t.Fatalf("stdin was not closed: %+v", status)
	}
}

func TestSessionReportsWrapperSetupFailure(t *testing.T) {
	r, _ := localSessions(t)
	req := phaseRequest(t, "exit 0")
	req.Command.StdinPath = filepath.Join(req.Command.Dir, "missing-input")
	ref, err := r.StartSession(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	status := awaitExit(t, r, ref)
	if status.ExitCode == nil || *status.ExitCode == 0 {
		t.Fatalf("setup failure not recorded: %+v", status)
	}
}

func TestSessionMissingWithoutExitMetadata(t *testing.T) {
	r, _ := localSessions(t)
	status, err := r.SessionStatus(context.Background(), runner.SessionRef{Name: "mergeyard-missing", PhaseDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if status.State != runner.SessionMissing {
		t.Fatalf("status = %+v", status)
	}
}

func awaitLog(t *testing.T, r runner.Runner, ref runner.SessionRef, text string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, err := r.ReadFile(context.Background(), filepath.Join(ref.PhaseDir, "events.jsonl"))
		if err == nil && strings.Contains(string(out), text) {
			return string(out)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("phase did not log %q", text)
	return ""
}

func TestStopEscalatesSignalsAndKillsUnresponsiveSession(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is required for session integration tests")
	}
	executable := filepath.Join(t.TempDir(), "fake-agent")
	if out, err := exec.Command("go", "build", "-o", executable, "./testdata/fakeagent").CombinedOutput(); err != nil {
		t.Fatalf("build fake agent: %v: %s", err, out)
	}
	for _, mode := range []string{"interrupt", "terminate", "kill"} {
		t.Run(mode, func(t *testing.T) {
			r, _ := localSessions(t)
			req := phaseRequest(t, "exit 0")
			req.Command.Executable, req.Command.Args = executable, []string{mode}
			ref, err := r.StartSession(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			awaitLog(t, r, ref, "ready")
			if err := r.StopSession(context.Background(), ref); err != nil {
				t.Fatal(err)
			}
			out, err := r.ReadFile(context.Background(), filepath.Join(ref.PhaseDir, "events.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			want := "ready\nSIGINT\n"
			if mode != "interrupt" {
				want += "SIGTERM\n"
			}
			if string(out) != want {
				t.Fatalf("signals = %q, want %q", out, want)
			}
			status, err := r.SessionStatus(context.Background(), ref)
			if err != nil {
				t.Fatal(err)
			}
			if status.State == runner.SessionRunning {
				t.Fatal("stopped session is still running")
			}
			if mode != "kill" && (status.State != runner.SessionExited || status.ExitCode == nil || *status.ExitCode != 0) {
				t.Fatalf("graceful exit not recorded: %+v", status)
			}
			if err := r.StopSession(context.Background(), ref); err != nil {
				t.Fatalf("repeat stop: %v", err)
			}
		})
	}
}

func TestSessionRecordsSignalExitAndWorktreeFailure(t *testing.T) {
	for _, scenario := range []string{"signal", "worktree"} {
		t.Run(scenario, func(t *testing.T) {
			r, _ := localSessions(t)
			req := phaseRequest(t, "kill -TERM $$")
			want := 143
			if scenario == "worktree" {
				req.Command.Dir = filepath.Join(req.Command.Dir, "missing")
				want = 1
			}
			ref, err := r.StartSession(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			status := awaitExit(t, r, ref)
			if status.ExitCode == nil || *status.ExitCode != want {
				t.Fatalf("status = %+v, want exit %d", status, want)
			}
		})
	}
}

func TestSessionNamesAreSafeBoundedAndDistinguishAttempts(t *testing.T) {
	r, _ := localSessions(t)
	req := phaseRequest(t, "while [ ! -f release ]; do sleep 0.02; done")
	req.RunID = strings.Repeat("run :.$' ", 50)
	req.Phase = strings.Repeat("review :.$' ", 50)
	ref, err := r.StartSession(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(ref.Name) > 96 {
		t.Fatalf("name too long: %q", ref.Name)
	}
	for _, ch := range ref.Name {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-') {
			t.Fatalf("unsafe name: %q", ref.Name)
		}
	}
	if _, err := r.StartSession(context.Background(), req); err == nil {
		t.Fatal("duplicate session was accepted")
	} else {
		var failure *fault.Error
		if !errors.As(err, &failure) || failure.Code != "phase.session_exists" {
			t.Fatalf("duplicate error = %v", err)
		}
	}
	if err := r.WriteFile(context.Background(), filepath.Join(req.Command.Dir, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	awaitExit(t, r, ref)
	req.Attempt++
	req.PhaseDir = t.TempDir()
	second, err := r.StartSession(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Name == second.Name {
		t.Fatal("attempts have the same name")
	}
	awaitExit(t, r, second)
}

func TestSessionStartsOnEmptyTmuxServer(t *testing.T) {
	r, options := localSessions(t)
	// Keep the server alive with no sessions to reproduce the interval between
	// the last phase exiting and tmux shutting down, without depending on timing.
	cmd := exec.Command("tmux", "-L", options.SocketName, "-f", "/dev/null",
		"start-server", ";", "set-option", "-g", "exit-empty", "off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("prepare empty tmux server: %v: %s", err, out)
	}
	ref, err := r.StartSession(context.Background(), phaseRequest(t, "exit 0"))
	if err != nil {
		t.Fatal(err)
	}
	status := awaitExit(t, r, ref)
	if status.ExitCode == nil || *status.ExitCode != 0 {
		t.Fatalf("status = %+v, want successful exit", status)
	}
}

func TestInvalidEnvironmentCannotExecuteShellText(t *testing.T) {
	r, _ := localSessions(t)
	req := phaseRequest(t, "exit 0")
	req.Command.Env = map[string]string{"BAD=KEY": "value"}
	if _, err := r.StartSession(context.Background(), req); err == nil {
		t.Fatal("invalid environment key was accepted")
	}
	req.Command.Env = nil
	req.Command.Args = []string{"NUL\x00argument"}
	if _, err := r.StartSession(context.Background(), req); err == nil {
		t.Fatal("NUL argument was accepted")
	}
}

func TestSessionRecordsLogRedirectionFailure(t *testing.T) {
	r, _ := localSessions(t)
	req := phaseRequest(t, "exit 0")
	if err := os.MkdirAll(filepath.Join(req.PhaseDir, "events.jsonl"), 0700); err != nil {
		t.Fatal(err)
	}
	ref, err := r.StartSession(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	status := awaitExit(t, r, ref)
	if status.ExitCode == nil || *status.ExitCode == 0 {
		t.Fatalf("log setup failure not recorded: %+v", status)
	}
}

func TestStopContinuesEscalationAfterParentExits(t *testing.T) {
	r, _ := localSessions(t)
	executable := filepath.Join(t.TempDir(), "fake-agent")
	if out, err := exec.Command("go", "build", "-o", executable, "./testdata/fakeagent").CombinedOutput(); err != nil {
		t.Fatalf("build fake agent: %v: %s", err, out)
	}
	req := phaseRequest(t, "exit 0")
	req.Command.Executable, req.Command.Args = executable, []string{"parent"}
	ref, err := r.StartSession(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	awaitLog(t, r, ref, "child ready")
	if err := r.StopSession(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	out, err := r.ReadFile(context.Background(), filepath.Join(ref.PhaseDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "child SIGTERM") {
		t.Fatalf("escalation stopped when parent exited: %q", out)
	}
	status := awaitExit(t, r, ref)
	if status.ExitCode == nil || *status.ExitCode != 0 {
		t.Fatalf("parent exit was not preserved: %+v", status)
	}
}

func TestStartRecoversSessionCreatedBeforeClientCancellation(t *testing.T) {
	r, _ := localSessions(t)
	realTmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	proxyDir := t.TempDir()
	marker := filepath.Join(proxyDir, "created")
	t.Setenv("REAL_TMUX", realTmux)
	t.Setenv("CREATED_MARKER", marker)
	proxy := `#!/bin/sh
for arg in "$@"; do
 if [ "$arg" = new-session ]; then
  "$REAL_TMUX" "$@" || exit $?
  : >"$CREATED_MARKER"
  exec /bin/sleep 3
 fi
done
exec "$REAL_TMUX" "$@"
`
	if err := os.WriteFile(filepath.Join(proxyDir, "tmux"), []byte(proxy), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", proxyDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(marker); err == nil {
				cancel()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	req := phaseRequest(t, "printf 'ready\\n'; while [ ! -f release ]; do sleep 0.02; done")
	ref, err := r.StartSession(ctx, req)
	if err != nil {
		t.Fatalf("created session became untracked: ref=%+v, err=%v", ref, err)
	}
	t.Cleanup(func() { r.StopSession(context.Background(), ref) })
	status, err := r.SessionStatus(context.Background(), ref)
	if err != nil || status.State != runner.SessionRunning {
		t.Fatalf("recovered status=%+v, err=%v", status, err)
	}
	if err := r.WriteFile(context.Background(), filepath.Join(req.Command.Dir, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	awaitExit(t, r, ref)
}
