package runner

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/sessions"
)

// Options configures the local tmux adapter; it is not part of Runner's contract.
type Options struct {
	SocketName  string
	GracePeriod time.Duration
}

// Local runs commands on this machine.
type Local struct{ sessions *sessions.Manager }

func NewLocal(options Options) *Local {
	return &Local{sessions: sessions.New(sessions.Options{SocketName: options.SocketName, GracePeriod: options.GracePeriod})}
}

// Exec reports non-zero process exits in ExitCode; launch and cancellation
// failures are errors. It captures stdout and stderr separately.
func (l *Local) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, req.Executable, req.Args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		} else {
			return err
		}
	}
	// Detached descendants may escape the group while retaining output pipes.
	// Bound pipe draining as well as the lifetime of the direct child.
	cmd.WaitDelay = time.Second
	cmd.Dir, cmd.Env = req.Dir, req.Environment()
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if req.StdinPath != "" {
		input, err := os.Open(req.StdinPath)
		if err != nil {
			return ExecResult{}, localFailure("internal.exec_input_failed", req.StdinPath, err)
		}
		defer input.Close()
		cmd.Stdin = input
	}
	err := cmd.Run()
	var cleanupErr error
	// Wait may prefer a nonzero parent exit over ErrWaitDelay. Clean up every
	// failed invocation rather than relying on that sentinel to detect pipes.
	if err != nil && cmd.Process != nil {
		cleanupErr = cmd.Cancel()
		if errors.Is(cleanupErr, os.ErrProcessDone) {
			cleanupErr = nil
		}
	}
	result := ExecResult{Stdout: out.Bytes(), Stderr: errOut.Bytes()}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if err != nil && ctx.Err() != nil {
		return result, localFailure("internal.canceled", req.Executable, errors.Join(ctx.Err(), cleanupErr))
	}
	if cleanupErr != nil {
		return result, localFailure("internal.exec_failed", req.Executable, errors.Join(err, cleanupErr))
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return result, nil
	}
	return result, localFailure("internal.exec_failed", req.Executable, err)
}

func (l *Local) StartSession(ctx context.Context, req SessionRequest) (SessionRef, error) {
	return l.sessions.Start(ctx, req)
}
func (l *Local) SessionStatus(ctx context.Context, ref SessionRef) (SessionStatus, error) {
	return l.sessions.Status(ctx, ref)
}
func (l *Local) StopSession(ctx context.Context, ref SessionRef) error {
	return l.sessions.Stop(ctx, ref)
}
func (l *Local) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, localFailure("internal.canceled", path, err)
	}
	data, err := os.ReadFile(path)
	return data, localFailure("internal.file_read_failed", path, err)
}
func (l *Local) WriteFile(ctx context.Context, path string, data []byte, mode fs.FileMode) error {
	if err := ctx.Err(); err != nil {
		return localFailure("internal.canceled", path, err)
	}
	return localFailure("internal.file_write_failed", path, os.WriteFile(path, data, mode))
}
func (l *Local) RemovePath(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return localFailure("internal.canceled", path, err)
	}
	return localFailure("internal.file_remove_failed", path, os.RemoveAll(path))
}

func localFailure(code, path string, err error) error {
	if err == nil {
		return nil
	}
	return &fault.Error{Code: code, Path: path, Err: err}
}

var _ Runner = (*Local)(nil)
