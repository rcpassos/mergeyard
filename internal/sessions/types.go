package sessions

import (
	"sort"
	"time"
)

// Command is an executable invocation. Env is the complete child environment;
// it never augments the control plane's environment. Empty StdinPath means EOF.
type Command struct {
	Executable string
	Args       []string
	Dir        string
	Env        map[string]string
	StdinPath  string
}

// Environment returns the explicit environment in stable order.
func (c Command) Environment() []string {
	env := make([]string, 0, len(c.Env))
	for key, value := range c.Env {
		env = append(env, key+"="+value)
	}
	sort.Strings(env)
	return env
}

// Request identifies a single phase attempt and where its logs are kept.
type Request struct {
	RunID    string
	Phase    string
	Round    int
	Attempt  int
	PhaseDir string
	Command  Command
}

// Ref can be persisted and used by a new manager after a control-plane restart.
type Ref struct {
	Name     string
	PhaseDir string
}

type State string

const (
	Running State = "running"
	Exited  State = "exited"
	Missing State = "missing"
)

type Status struct {
	State    State
	ExitCode *int
}

// Options selects the tmux server and stop timing. Zero values use a dedicated
// mergeyard socket and a five-second grace period for each signal.
type Options struct {
	SocketName  string
	GracePeriod time.Duration
}
