// Package runner executes phases for a run.
package runner

import (
	"context"
	"io/fs"

	"github.com/rcpassos/mergeyard/internal/execution"
)

type ExecRequest = execution.Command
type SessionRequest = execution.SessionRequest
type SessionRef = execution.SessionRef
type SessionStatus = execution.SessionStatus
type SessionState = execution.SessionState

const (
	SessionRunning = execution.Running
	SessionExited  = execution.Exited
	SessionMissing = execution.Missing
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
	// StartSession returns a nonzero ref if a launch may have committed, even
	// when reconciliation fails. Persist that ref before retrying the attempt.
	StartSession(context.Context, SessionRequest) (SessionRef, error)
	SessionStatus(context.Context, SessionRef) (SessionStatus, error)
	// ListSessions inventories managed sessions on this runner's tmux socket.
	ListSessions(context.Context) ([]string, error)
	StopSession(context.Context, SessionRef) error
	ReadFile(context.Context, string) ([]byte, error)
	WriteFile(context.Context, string, []byte, fs.FileMode) error
	RemovePath(context.Context, string) error
}
