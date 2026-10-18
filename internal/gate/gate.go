// Package gate serializes work while honoring cancellation: a caller that gives
// up, disconnects, or shuts down is released instead of waiting for a held gate.
package gate

import (
	"context"
	"sync"
)

// Gate admits one holder at a time. The zero value is ready to use.
type Gate struct {
	init  sync.Once
	token chan struct{}
}

// Lock acquires the gate, or returns the context's error when the caller gives
// up first. Callers must call Unlock exactly once after a nil result.
func (g *Gate) Lock(ctx context.Context) error {
	// Check first so an abandoned caller never takes a free gate.
	if err := ctx.Err(); err != nil {
		return err
	}
	g.init.Do(func() { g.token = make(chan struct{}, 1) })
	select {
	case g.token <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Unlock releases a gate held by the calling goroutine.
func (g *Gate) Unlock() { <-g.token }
