package sessions

import (
	"github.com/rcpassos/mergeyard/internal/execution"
	"time"
)

type Command = execution.Command
type Request = execution.SessionRequest
type Ref = execution.SessionRef
type State = execution.SessionState
type Status = execution.SessionStatus

const (
	Running = execution.Running
	Exited  = execution.Exited
	Missing = execution.Missing
)

// Options selects the tmux server and stop timing. Zero values use a dedicated
// mergeyard socket and a five-second grace period for each signal.
type Options struct {
	SocketName  string
	GracePeriod time.Duration
}
