package gate_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/gate"
)

func TestGateSerializesHolders(t *testing.T) {
	var g gate.Gate
	if err := g.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := make(chan error, 1)
	go func() { second <- g.Lock(context.Background()) }()
	select {
	case err := <-second:
		t.Fatalf("second holder entered without release: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	g.Unlock()
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("released gate never admitted the waiting holder")
	}
	g.Unlock()
}

func TestGateReleasesACancelledWaiter(t *testing.T) {
	var g gate.Gate
	if err := g.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := g.Lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cancelled waiter blocked for %s", elapsed)
	}
	g.Unlock()
}

func TestGateRejectsAnAbandonedCaller(t *testing.T) {
	var g gate.Gate
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Even a free gate is not entered by a caller that already gave up.
	if err := g.Lock(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("lock error = %v", err)
	}
}
