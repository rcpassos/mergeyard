package testkit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/runner"
)

type FailFirstImplementLaunch struct {
	runner.Runner
	Failed bool
}

func (r *FailFirstImplementLaunch) StartSession(ctx context.Context, req runner.SessionRequest) (runner.SessionRef, error) {
	if req.Phase == "implement" && !r.Failed {
		r.Failed = true
		return runner.SessionRef{}, errors.New("implementer executable unavailable")
	}
	return r.Runner.StartSession(ctx, req)
}

func SetupWorktree(t *testing.T, rt *app.Runtime, id string) string {
	t.Helper()
	var tree string
	// Workspace identity is test fixture wiring, not a behavior assertion.
	if err := rt.DB.QueryRow("SELECT worktree_path FROM runs WHERE id=?", id).Scan(&tree); err != nil {
		t.Fatal(err)
	}
	return tree
}
func AwaitSetupSession(t *testing.T, r runner.Runner, ref runner.SessionRef) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, err := r.SessionStatus(context.Background(), ref)
		if err != nil {
			t.Fatal(err)
		}
		if status.State == runner.SessionExited {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("setup did not exit")
}
