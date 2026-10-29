package scheduler_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
)

func TestHarnessCheckWaitHonorsCancellation(t *testing.T) {
	t.Parallel()

	flow := testkit.NewScenario(t)
	_, _, api, _, cfg, r := flow.Parts()
	held := &testkit.HeldGitHub{FakeGitHub: api, Entered: make(chan struct{}), Release: make(chan struct{})}
	s, err := flow.New(cfg, scheduler.Dependencies{GitHub: held, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	planning := make(chan error, 1)
	go func() {
		_, err := s.Plan(context.Background(), "")
		planning <- err
	}()
	<-held.Entered
	// The tick gate is held. Check availability must return on the caller's
	// cancellation instead of waiting for the stalled tick.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	checking := make(chan error, 1)
	go func() {
		_, err := s.CheckHarness(ctx, "claude")
		checking <- err
	}()
	select {
	case err := <-checking:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("harness check error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Check availability ignored cancellation while the tick gate was held")
	}
	close(held.Release)
	if err := <-planning; err != nil {
		t.Fatal(err)
	}
}
