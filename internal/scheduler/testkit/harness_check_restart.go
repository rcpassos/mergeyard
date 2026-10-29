package testkit

import (
	"context"
	"fmt"

	"github.com/rcpassos/mergeyard/internal/runner"
)

type HarnessCheckCrashRunner struct{ runner.Runner }

func (r HarnessCheckCrashRunner) StartSession(ctx context.Context, req runner.SessionRequest) (runner.SessionRef, error) {
	ref, err := r.Runner.StartSession(ctx, req)
	if err == nil && req.Phase == "harness-check" {
		fmt.Println("review-control-plane-ready")
		select {}
	}
	return ref, err
}
