package testkit

import (
	"context"
	"os"

	"github.com/rcpassos/mergeyard/internal/runner"
)

// Delay the second harness until Check starts: tmux accepting a session does
// not mean its process has opened the executable yet. Exercise that ordering.
type DelayedHarnessStartRunner struct {
	runner.Runner
	ImplementStarts int
	Gate            string
}

func (r *DelayedHarnessStartRunner) StartSession(ctx context.Context, req runner.SessionRequest) (runner.SessionRef, error) {
	if req.Phase == "implement" {
		r.ImplementStarts++
		if r.ImplementStarts == 2 {
			original := req.Command
			req.Command.Executable = "/bin/sh"
			req.Command.Args = []string{"-c", `while [ ! -f "$1" ]; do sleep 0.01; done; shift; exec "$@"`, "delayed-harness", r.Gate, original.Executable}
			req.Command.Args = append(req.Command.Args, original.Args...)
		}
	}
	if req.Phase == "harness-check" {
		if err := os.WriteFile(r.Gate, []byte("go"), 0600); err != nil {
			return runner.SessionRef{}, err
		}
	}
	return r.Runner.StartSession(ctx, req)
}
