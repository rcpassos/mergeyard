package testkit

import (
	"context"
	"fmt"

	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type RecoveryCrashRunner struct {
	runner.Runner
	Phase workflow.Phase
	Mode  string
}

func (r RecoveryCrashRunner) StartSession(ctx context.Context, req runner.SessionRequest) (runner.SessionRef, error) {
	replacement := req.Phase == string(r.Phase) && req.Attempt == 2
	if replacement && r.Mode == "before-launch" {
		fmt.Println("review-control-plane-ready")
		select {}
	}
	ref, err := r.Runner.StartSession(ctx, req)
	if err == nil && replacement && r.Mode == "after-launch" {
		fmt.Println("review-control-plane-ready")
		select {}
	}
	return ref, err
}
