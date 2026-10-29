package testkit

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/runner"
)

// Keep real process ownership and stopping; only the status observation fails.
type InvalidReviewStatus struct {
	runner.Runner
	Code  string
	Stops int
}

func (r *InvalidReviewStatus) SessionStatus(ctx context.Context, ref runner.SessionRef) (runner.SessionStatus, error) {
	status, err := r.Runner.SessionStatus(ctx, ref)
	if err == nil && r.Code != "" && strings.HasPrefix(filepath.Base(ref.PhaseDir), "review-") && status.State == runner.SessionExited {
		return runner.SessionStatus{}, &fault.Error{Code: r.Code, Message: "review process metadata is unreadable"}
	}
	return status, err
}
func (r *InvalidReviewStatus) StopSession(ctx context.Context, ref runner.SessionRef) error {
	if strings.HasPrefix(filepath.Base(ref.PhaseDir), "review-") {
		r.Stops++
	}
	return r.Runner.StopSession(ctx, ref)
}
