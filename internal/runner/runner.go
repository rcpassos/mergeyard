// Package runner executes phases for a run.
package runner

import (
	"context"
	"io/fs"

	"github.com/rcpassos/mergeyard/internal/sessions"
)

type ExecRequest = sessions.Command
type SessionRequest = sessions.Request
type SessionRef = sessions.Ref
type SessionStatus = sessions.Status
type Options = sessions.Options

const (
	SessionRunning = sessions.Running
	SessionExited  = sessions.Exited
	SessionMissing = sessions.Missing
)

type ExecResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Runner is the execution boundary used by the workflow. Cancelling a method's
// context cancels that operation, not a previously started phase session.
type Runner interface {
	Exec(context.Context, ExecRequest) (ExecResult, error)
	StartSession(context.Context, SessionRequest) (SessionRef, error)
	SessionStatus(context.Context, SessionRef) (SessionStatus, error)
	StopSession(context.Context, SessionRef) error
	ReadFile(context.Context, string) ([]byte, error)
	WriteFile(context.Context, string, []byte, fs.FileMode) error
	RemovePath(context.Context, string) error
}
