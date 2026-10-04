// Package execution defines the transport-independent phase execution contract.
package execution

import "sort"

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

// SessionRequest identifies a single phase attempt and where its logs are kept.
type SessionRequest struct {
	RunID    string
	Phase    string
	Round    int
	Attempt  int
	PhaseDir string
	Command  Command
}

// SessionRef can be persisted and used by a new manager after a control-plane restart.
type SessionRef struct {
	Name     string
	PhaseDir string
}

type SessionState string

const (
	Running SessionState = "running"
	Exited  SessionState = "exited"
	Missing SessionState = "missing"
)

type SessionStatus struct {
	State    SessionState
	ExitCode *int
}
