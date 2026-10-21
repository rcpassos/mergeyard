// Package proc runs one bounded external command with a single lifetime policy:
// the command owns its process group, a failed or over-bounded command stops
// that group, and output draining is bounded so a descendant that keeps the
// pipes open cannot hold the caller.
package proc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// defaultWaitDelay bounds output draining after the process exits or its
// context ends.
const defaultWaitDelay = time.Second

// Request describes one invocation. Env is the complete child environment.
type Request struct {
	Executable string
	Args       []string
	Dir        string
	Env        []string
	Stdin      io.Reader
	// WaitDelay bounds output draining; zero uses the default.
	WaitDelay time.Duration
	// StdoutLimit keeps at most this many bytes of output and reads the rest
	// without keeping it; zero keeps everything.
	StdoutLimit int
}

// Result reports the captured output and exit of one invocation.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Run executes req and captures its output. Errors follow os/exec: a nonzero
// exit yields *exec.ExitError, and a context that ends first yields that
// context's error. A successful command whose descendant keeps the output pipes
// open yields exec.ErrWaitDelay with ExitCode 0. Every outcome except a clean
// drain stops the owned process group first, so no descendant outlives the
// call.
func Run(ctx context.Context, req Request) (Result, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, req.Executable, req.Args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		} else {
			return err
		}
	}
	waitDelay := req.WaitDelay
	if waitDelay <= 0 {
		waitDelay = defaultWaitDelay
	}
	// Detached descendants may escape the group while retaining output pipes.
	// Bound pipe draining as well as the lifetime of the direct child.
	cmd.WaitDelay = waitDelay
	cmd.Dir, cmd.Env, cmd.Stdin = req.Dir, req.Env, req.Stdin
	cmd.Stdout, cmd.Stderr = io.Writer(&stdout), &stderr
	if req.StdoutLimit > 0 {
		cmd.Stdout = &limitedWriter{buf: &stdout, limit: req.StdoutLimit}
	}
	err := cmd.Run()
	result := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	// Wait may prefer a nonzero parent exit over ErrWaitDelay. Stop the group
	// after any failure so a descendant cannot outlive the command.
	if err != nil && cmd.Process != nil {
		if cleanup := cmd.Cancel(); cleanup != nil && !errors.Is(cleanup, os.ErrProcessDone) {
			err = errors.Join(err, cleanup)
		}
	}
	if err != nil && ctx.Err() != nil {
		// Callers cancel a command through its context; keep that cause
		// inspectable alongside the killed process.
		err = errors.Join(err, ctx.Err())
	}
	return result, err
}

// limitedWriter keeps the first limit bytes written and drops the rest, while
// reporting every write as complete so the command is never blocked or failed
// by the cap.
type limitedWriter struct {
	buf   *bytes.Buffer
	limit int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if room := w.limit - w.buf.Len(); room > 0 {
		w.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}
