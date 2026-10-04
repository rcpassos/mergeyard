package scheduler

import (
	"context"
	"sync"

	"github.com/rcpassos/mergeyard/internal/events"
)

// Control owns the runtime's global claim gate. Discovery and dispatch must
// consult Paused before claiming; existing runs are unaffected by this gate.
type Control struct {
	mu     sync.Mutex
	paused bool
	bus    *events.Bus
}

func NewControl(bus *events.Bus) *Control { return &Control{bus: bus} }

func (c *Control) Paused() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paused
}

func (c *Control) Pause(ctx context.Context) error  { return c.setPaused(ctx, true) }
func (c *Control) Resume(ctx context.Context) error { return c.setPaused(ctx, false) }

func (c *Control) setPaused(ctx context.Context, paused bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused == paused {
		return nil
	}
	name := "scheduler.resumed"
	if paused {
		name = "scheduler.paused"
	}
	if _, err := c.bus.Publish(ctx, events.Draft{Type: name, Payload: struct {
		Paused bool `json:"paused"`
	}{paused}}); err != nil {
		return err
	}
	c.paused = paused
	return nil
}
