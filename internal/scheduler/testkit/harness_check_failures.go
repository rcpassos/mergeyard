package testkit

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/rcpassos/mergeyard/internal/runner"
)

type CheckFailureRunner struct {
	runner.Runner
	Output    string
	Ambiguous bool
}

func (r CheckFailureRunner) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if strings.Contains(path, "harness-checks") && filepath.Base(path) == r.Output {
		return nil, errors.New("captured output unavailable")
	}
	return r.Runner.ReadFile(ctx, path)
}
func (r CheckFailureRunner) SessionStatus(ctx context.Context, ref runner.SessionRef) (runner.SessionStatus, error) {
	if r.Ambiguous && strings.Contains(ref.PhaseDir, "harness-checks") {
		return runner.SessionStatus{}, errors.New("process identity unavailable")
	}
	return r.Runner.SessionStatus(ctx, ref)
}
