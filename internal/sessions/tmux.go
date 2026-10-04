// Package sessions manages tmux process sessions.
package sessions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// Manager keeps no process handles; refs and tmux are sufficient after restart.
type Manager struct{ options Options }

func New(options Options) *Manager {
	if options.SocketName == "" {
		options.SocketName = "mergeyard"
	}
	if options.GracePeriod <= 0 {
		options.GracePeriod = 5 * time.Second
	}
	return &Manager{options: options}
}

func (m *Manager) tmux(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "tmux", append([]string{"-L", m.options.SocketName, "-f", "/dev/null"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil && ctx.Err() != nil {
		return out, failure("phase.canceled", m.options.SocketName, ctx.Err())
	}
	if err != nil {
		code := "phase.tmux_failed"
		var launchErr *exec.Error
		if errors.As(err, &launchErr) {
			code = "phase.tmux_unavailable"
		}
		return out, failure(code, m.options.SocketName, fmt.Errorf("tmux: %w: %s", err, strings.TrimSpace(string(out))))
	}
	return out, nil
}

func (m *Manager) exists(ctx context.Context, name string) (bool, error) {
	out, err := m.tmux(ctx, "has-session", "-t", "="+name)
	// The last pane can exit while a client is connecting. Confirm absence on
	// a fresh connection rather than treating that transport race as failure.
	if err != nil && strings.Contains(string(out), "server exited unexpectedly") && ctx.Err() == nil {
		out, err = m.tmux(ctx, "has-session", "-t", "="+name)
	}
	if err == nil {
		return true, nil
	}
	if ctx.Err() != nil {
		return false, failure("phase.canceled", name, ctx.Err())
	}
	text := string(out)
	if strings.Contains(text, "can't find session") || strings.Contains(text, "no server running") || strings.Contains(text, "No such file or directory") {
		return false, nil
	}
	return false, err
}

// Name is bounded and collision-resistant even when run IDs share a prefix or
// contain characters tmux treats as target syntax.
func Name(req Request) string {
	sanitize := func(s string, limit int) string {
		var b strings.Builder
		for _, r := range s {
			if b.Len() >= limit {
				break
			}
			if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-') {
				b.WriteRune(r)
			} else {
				b.WriteByte('-')
			}
		}
		return b.String()
	}
	hash := sha256.Sum256([]byte(req.RunID))
	short := sanitize(req.RunID, 12) + "-" + hex.EncodeToString(hash[:4])
	return fmt.Sprintf("mergeyard-%s-%s-%d-%d", short, sanitize(req.Phase, 16), req.Round, req.Attempt)
}

func (m *Manager) Start(ctx context.Context, req Request) (Ref, error) {
	if err := ctx.Err(); err != nil {
		return Ref{}, failure("phase.canceled", "", err)
	}
	if req.RunID == "" || req.Phase == "" || req.Round < 0 || req.Attempt < 1 {
		return Ref{}, failure("phase.invalid_request", "", errors.New("session requires run ID, phase, nonnegative round, and positive attempt"))
	}
	if req.PhaseDir == "" || req.Command.Dir == "" {
		return Ref{}, failure("phase.invalid_request", "", errors.New("session requires phase directory and worktree"))
	}
	phaseDir, err := filepath.Abs(req.PhaseDir)
	if err != nil {
		return Ref{}, failure("phase.invalid_request", req.PhaseDir, err)
	}
	req.Command.Dir, err = filepath.Abs(req.Command.Dir)
	if err != nil {
		return Ref{}, failure("phase.invalid_request", req.Command.Dir, err)
	}
	if req.Command.StdinPath != "" {
		req.Command.StdinPath, err = filepath.Abs(req.Command.StdinPath)
		if err != nil {
			return Ref{}, failure("phase.invalid_request", req.Command.StdinPath, err)
		}
	}
	executable, err := exec.LookPath(req.Command.Executable)
	if err != nil {
		return Ref{}, failure("phase.executable_unavailable", req.Command.Executable, err)
	}
	req.Command.Executable, err = filepath.Abs(executable)
	if err != nil {
		return Ref{}, failure("phase.invalid_request", executable, err)
	}
	script, err := wrapper(req.Command, phaseDir)
	if err != nil {
		return Ref{}, err
	}
	ref := Ref{Name: Name(req), PhaseDir: phaseDir}
	exists, err := m.exists(ctx, ref.Name)
	if err != nil {
		return Ref{}, err
	}
	if exists {
		return Ref{}, failure("phase.session_exists", ref.Name, errors.New("session already exists"))
	}
	if err := os.MkdirAll(phaseDir, 0700); err != nil {
		return Ref{}, failure("phase.wrapper_write_failed", phaseDir, err)
	}
	path := filepath.Join(phaseDir, "wrapper.sh")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		code := "phase.wrapper_write_failed"
		if errors.Is(err, os.ErrExist) {
			code = "phase.attempt_exists"
		}
		return Ref{}, failure(code, path, err)
	}
	_, writeErr := file.WriteString(script)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return Ref{}, failure("phase.wrapper_write_failed", path, err)
	}
	// Multiple shell-command arguments make tmux exec directly, without parsing
	// the wrapper path as a shell command.
	if _, err := m.tmux(ctx, "new-session", "-d", "-s", ref.Name, "--", "/bin/sh", path); err != nil {
		// The server may have committed the launch before its client was
		// cancelled. Reconcile independently of the cancelled caller context.
		probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		status, probeErr := m.Status(probeCtx, ref)
		if probeErr == nil && status.State != Missing {
			return ref, nil
		}
		// Preserve the attempt identity even when the outcome is uncertain.
		return ref, failure("phase.session_start_failed", ref.Name, errors.Join(err, probeErr))
	}
	return ref, nil
}

func (m *Manager) Status(ctx context.Context, ref Ref) (Status, error) {
	if err := ctx.Err(); err != nil {
		return Status{}, failure("phase.canceled", ref.Name, err)
	}
	data, err := os.ReadFile(filepath.Join(ref.PhaseDir, "exit.json"))
	if err == nil {
		var metadata struct {
			ExitCode *int `json:"exit_code"`
		}
		if err := json.Unmarshal(data, &metadata); err != nil {
			return Status{}, failure("phase.exit_invalid", filepath.Join(ref.PhaseDir, "exit.json"), err)
		}
		if metadata.ExitCode == nil {
			return Status{}, failure("phase.exit_invalid", filepath.Join(ref.PhaseDir, "exit.json"), errors.New("exit metadata has no exit_code"))
		}
		return Status{State: Exited, ExitCode: metadata.ExitCode}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Status{}, failure("phase.exit_read_failed", filepath.Join(ref.PhaseDir, "exit.json"), err)
	}
	exists, err := m.exists(ctx, ref.Name)
	if err != nil {
		return Status{}, err
	}
	if exists {
		return Status{State: Running}, nil
	}
	// A fast phase may have exited between reading metadata and querying tmux.
	if _, err := os.Stat(filepath.Join(ref.PhaseDir, "exit.json")); err == nil {
		return m.Status(ctx, ref)
	}
	return Status{State: Missing}, nil
}

// Stop signals the whole pane process group, including the harness's children.
// A forced kill cannot be observed by the wrapper, so it may leave no metadata.
func (m *Manager) Stop(ctx context.Context, ref Ref) error {
	exists, err := m.exists(ctx, ref.Name)
	if err != nil || !exists {
		return err
	}
	pid, err := m.panePID(ctx, ref)
	if err != nil {
		return m.stopError(ctx, ref, err)
	}
	for _, signal := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		if err := ctx.Err(); err != nil {
			return failure("phase.canceled", ref.Name, err)
		}
		if err := syscall.Kill(-pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
			return failure("phase.stop_failed", ref.Name, err)
		}
		stopped, err := m.waitStopped(ctx, ref, pid)
		if err != nil || stopped {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return failure("phase.canceled", ref.Name, err)
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return failure("phase.stop_failed", ref.Name, err)
	}
	_, err = m.tmux(ctx, "kill-session", "-t", "="+ref.Name)
	return m.stopError(ctx, ref, err)
}

// A concurrent natural exit makes a failed stop command harmless.
func (m *Manager) stopError(ctx context.Context, ref Ref, err error) error {
	if err != nil && ctx.Err() == nil {
		if exists, checkErr := m.exists(ctx, ref.Name); checkErr == nil && !exists {
			return nil
		}
	}
	return err
}

func (m *Manager) waitStopped(ctx context.Context, ref Ref, pid int) (bool, error) {
	deadline := time.NewTimer(m.options.GracePeriod)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		// The wrapper can finish before its children. Keep escalating until the
		// process group is gone, even if tmux has already removed the session.
		groupErr := syscall.Kill(-pid, 0)
		// EPERM from signal 0 still establishes that the group exists. Keep
		// waiting; an actual signal failure is handled by Stop itself.
		if groupErr != nil && !errors.Is(groupErr, syscall.ESRCH) && !errors.Is(groupErr, syscall.EPERM) {
			return false, failure("phase.stop_failed", ref.Name, groupErr)
		}
		if errors.Is(groupErr, syscall.ESRCH) {
			exists, err := m.exists(ctx, ref.Name)
			if err != nil || !exists {
				return !exists, err
			}
		}
		select {
		case <-ctx.Done():
			return false, failure("phase.canceled", ref.Name, ctx.Err())
		case <-deadline.C:
			return false, nil
		case <-tick.C:
		}
	}
}

func quote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

func wrapper(command Command, phaseDir string) (string, error) {
	// env consumes operands containing '=' as assignments, even after '--'.
	if strings.Contains(command.Executable, "=") {
		return "", failure("phase.invalid_request", command.Executable, errors.New("executable path cannot contain '='"))
	}
	values := append([]string{command.Executable, command.Dir, command.StdinPath, phaseDir}, command.Args...)
	values = append(values, command.Environment()...)
	for _, value := range values {
		if strings.ContainsRune(value, 0) {
			return "", failure("phase.invalid_request", "", errors.New("command contains NUL"))
		}
	}
	for key := range command.Env {
		if key == "" {
			return "", failure("phase.invalid_request", "", errors.New("empty environment key"))
		}
		for i, r := range key {
			if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
				return "", failure("phase.invalid_request", key, errors.New("invalid environment key"))
			}
		}
	}
	input := command.StdinPath
	if input == "" {
		input = "/dev/null"
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	fmt.Fprintf(&b, "finish() {\n code=$1\n trap - EXIT\n printf '{\"exit_code\":%%s}\\n' \"$code\" >%s &&\n /bin/mv -f %s %s\n exit \"$code\"\n}\ntrap 'finish \"$?\"' EXIT\ntrap ':' INT TERM HUP\n", quote(filepath.Join(phaseDir, "exit.json.tmp")), quote(filepath.Join(phaseDir, "exit.json.tmp")), quote(filepath.Join(phaseDir, "exit.json")))
	fmt.Fprintf(&b, "exec >%s 2>%s\n", quote(filepath.Join(phaseDir, "events.jsonl")), quote(filepath.Join(phaseDir, "stderr.log")))
	fmt.Fprintf(&b, "cd %s || exit 1\n/usr/bin/env -i", quote(command.Dir))
	for _, value := range command.Environment() {
		fmt.Fprintf(&b, " %s", quote(value))
	}
	fmt.Fprintf(&b, " %s", quote(command.Executable))
	for _, arg := range command.Args {
		fmt.Fprintf(&b, " %s", quote(arg))
	}
	fmt.Fprintf(&b, " <%s\nexit $?\n", quote(input))
	return b.String(), nil
}

// panePID is the wrapper's process group leader when started directly by tmux.
func (m *Manager) panePID(ctx context.Context, ref Ref) (int, error) {
	out, err := m.tmux(ctx, "display-message", "-p", "-t", "="+ref.Name+":", "#{pane_pid}")
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 1 {
		return 0, failure("phase.invalid_pid", ref.Name, fmt.Errorf("invalid tmux pane PID %q", out))
	}
	return pid, nil
}
