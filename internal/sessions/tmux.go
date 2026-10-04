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
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	if err != nil {
		return out, fmt.Errorf("tmux: %w: %s", err, strings.TrimSpace(string(out)))
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
		return false, ctx.Err()
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
		return Ref{}, err
	}
	if req.RunID == "" || req.Phase == "" || req.Round < 0 || req.Attempt < 1 {
		return Ref{}, errors.New("session requires run ID, phase, nonnegative round, and positive attempt")
	}
	if req.PhaseDir == "" || req.Command.Dir == "" {
		return Ref{}, errors.New("session requires phase directory and worktree")
	}
	phaseDir, err := filepath.Abs(req.PhaseDir)
	if err != nil {
		return Ref{}, err
	}
	req.Command.Dir, err = filepath.Abs(req.Command.Dir)
	if err != nil {
		return Ref{}, err
	}
	if req.Command.StdinPath != "" {
		req.Command.StdinPath, err = filepath.Abs(req.Command.StdinPath)
		if err != nil {
			return Ref{}, err
		}
	}
	executable, err := exec.LookPath(req.Command.Executable)
	if err != nil {
		return Ref{}, err
	}
	req.Command.Executable, err = filepath.Abs(executable)
	if err != nil {
		return Ref{}, err
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
		return Ref{}, fmt.Errorf("session %s already exists", ref.Name)
	}
	if err := os.MkdirAll(phaseDir, 0700); err != nil {
		return Ref{}, err
	}
	path := filepath.Join(phaseDir, "wrapper.sh")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		return Ref{}, fmt.Errorf("create phase wrapper: %w", err)
	}
	_, writeErr := file.WriteString(script)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return Ref{}, err
	}
	// Multiple shell-command arguments make tmux exec directly, without parsing
	// the wrapper path as a shell command.
	if _, err := m.tmux(ctx, "new-session", "-d", "-s", ref.Name, "--", "/bin/sh", path); err != nil {
		return Ref{}, err
	}
	return ref, nil
}

func (m *Manager) Status(ctx context.Context, ref Ref) (Status, error) {
	if err := ctx.Err(); err != nil {
		return Status{}, err
	}
	data, err := os.ReadFile(filepath.Join(ref.PhaseDir, "exit.json"))
	if err == nil {
		var metadata struct {
			ExitCode *int `json:"exit_code"`
		}
		if err := json.Unmarshal(data, &metadata); err != nil {
			return Status{}, fmt.Errorf("read exit metadata: %w", err)
		}
		if metadata.ExitCode == nil {
			return Status{}, errors.New("exit metadata has no exit_code")
		}
		return Status{State: Exited, ExitCode: metadata.ExitCode}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Status{}, err
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
			return err
		}
		if err := syscall.Kill(-pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		stopped, err := m.waitStopped(ctx, ref, pid)
		if err != nil || stopped {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
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
		if groupErr != nil && !errors.Is(groupErr, syscall.ESRCH) {
			return false, groupErr
		}
		if errors.Is(groupErr, syscall.ESRCH) {
			exists, err := m.exists(ctx, ref.Name)
			if err != nil || !exists {
				return !exists, err
			}
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-deadline.C:
			return false, nil
		case <-tick.C:
		}
	}
}

func quote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

func wrapper(command Command, phaseDir string) (string, error) {
	values := append([]string{command.Executable, command.Dir, command.StdinPath, phaseDir}, command.Args...)
	values = append(values, command.Environment()...)
	for _, value := range values {
		if strings.ContainsRune(value, 0) {
			return "", errors.New("command contains NUL")
		}
	}
	for key := range command.Env {
		if key == "" {
			return "", errors.New("empty environment key")
		}
		for i, r := range key {
			if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
				return "", fmt.Errorf("invalid environment key %q", key)
			}
		}
	}
	input := command.StdinPath
	if input == "" {
		input = "/dev/null"
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	fmt.Fprintf(&b, "finish() {\n code=$1\n trap - EXIT\n printf '{\"exit_code\":%%s}\\n' \"$code\" >%s\n /bin/mv -f %s %s\n exit \"$code\"\n}\ntrap 'finish \"$?\"' EXIT\ntrap ':' INT TERM HUP\n", quote(filepath.Join(phaseDir, "exit.json.tmp")), quote(filepath.Join(phaseDir, "exit.json.tmp")), quote(filepath.Join(phaseDir, "exit.json")))
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
		return 0, fmt.Errorf("invalid tmux pane PID %q", out)
	}
	return pid, nil
}
