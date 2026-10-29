package testkit

import (
	"context"
	"fmt"

	"github.com/rcpassos/mergeyard/internal/runner"
)

// The runner models an interrupted HTTP request after intent has been saved.
type InterruptedTakeoverRunner struct {
	runner.Runner
	Cancel context.CancelFunc
}

func (r InterruptedTakeoverRunner) StopSession(ctx context.Context, ref runner.SessionRef) error {
	r.Cancel()
	return ctx.Err()
}

type AmbiguousTakeoverRunner struct{ runner.Runner }

func (r AmbiguousTakeoverRunner) SessionStatus(context.Context, runner.SessionRef) (runner.SessionStatus, error) {
	return runner.SessionStatus{State: runner.SessionMissing}, nil
}

// This crash occurs after the real runner has proved exit and before the
// scheduler can journal the manual outcome.
type TakeoverCrashRunner struct{ runner.Runner }

func (r TakeoverCrashRunner) StopSession(ctx context.Context, ref runner.SessionRef) error {
	if err := r.Runner.StopSession(ctx, ref); err != nil {
		return err
	}
	fmt.Println("review-control-plane-ready")
	select {}
}
