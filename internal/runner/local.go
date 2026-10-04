package runner

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"

	"github.com/rcpassos/mergeyard/internal/sessions"
)

// Local runs commands on this machine.
type Local struct{ options Options }

func NewLocal(options Options) *Local { return &Local{options: options} }

// Exec reports non-zero process exits in ExitCode; launch and cancellation
// failures are errors. It captures stdout and stderr separately.
func (l *Local) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, req.Executable, req.Args...)
	cmd.Dir, cmd.Env = req.Dir, req.Environment()
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if req.StdinPath != "" {
		input, err := os.Open(req.StdinPath)
		if err != nil {
			return ExecResult{}, err
		}
		defer input.Close()
		cmd.Stdin = input
	}
	err := cmd.Run()
	result := ExecResult{Stdout: out.Bytes(), Stderr: errOut.Bytes()}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return result, nil
	}
	return result, err
}

func (l *Local) StartSession(ctx context.Context, req SessionRequest) (SessionRef, error) {
	return sessions.New(l.options).Start(ctx, req)
}
func (l *Local) SessionStatus(ctx context.Context, ref SessionRef) (SessionStatus, error) {
	return sessions.New(l.options).Status(ctx, ref)
}
func (l *Local) StopSession(ctx context.Context, ref SessionRef) error {
	return sessions.New(l.options).Stop(ctx, ref)
}
func (l *Local) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}
func (l *Local) WriteFile(ctx context.Context, path string, data []byte, mode fs.FileMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.WriteFile(path, data, mode)
}
func (l *Local) RemovePath(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.RemoveAll(path)
}

var _ Runner = (*Local)(nil)
