package runner

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"time"

	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/proc"
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

// Exec reports process exits in ExitCode and captures stdout and stderr separately.
// Launch and cancellation failures are errors. If a successful command leaves
// output pipes open past the drain limit, it returns internal.exec_output_incomplete
// with exit code 0 and the output captured so far. It stops remaining processes
// in the command's group before returning.
func (l *Local) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	var input io.Reader
	if req.StdinPath != "" {
		file, err := os.Open(req.StdinPath)
		if err != nil {
			return ExecResult{}, localFailure("internal.exec_input_failed", req.StdinPath, err)
		}
		defer file.Close()
		input = file
	}
	result, err := proc.Run(ctx, proc.Request{
		Executable: req.Executable, Args: req.Args, Dir: req.Dir, Env: req.Environment(), Stdin: input,
		WaitDelay: time.Second,
	})
	execResult := ExecResult{Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: result.ExitCode}
	switch {
	case err == nil:
		return execResult, nil
	case ctx.Err() != nil:
		return execResult, localFailure("internal.canceled", req.Executable, err)
	case errors.Is(err, exec.ErrWaitDelay) && result.ExitCode == 0:
		return execResult, localFailure("internal.exec_output_incomplete", req.Executable, err)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return execResult, nil
	}
	return execResult, localFailure("internal.exec_failed", req.Executable, err)
}

func (l *Local) StartSession(ctx context.Context, req SessionRequest) (SessionRef, error) {
	return l.sessions.Start(ctx, req)
}
func (l *Local) SessionStatus(ctx context.Context, ref SessionRef) (SessionStatus, error) {
	return l.sessions.Status(ctx, ref)
}
func (l *Local) ListSessions(ctx context.Context) ([]string, error) {
	return l.sessions.List(ctx)
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
