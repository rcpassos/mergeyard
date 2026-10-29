package testkit

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/runner"
)

// LocalRunner remembers launches and adopted process references. Cleanup uses
// the Runner contract, including journal locations returned after restart.
type LocalRunner struct {
	*runner.Local
	mu   sync.Mutex
	refs map[runner.SessionRef]struct{}
}

func NewLocalRunner(t *testing.T, options runner.Options) *LocalRunner {
	t.Helper()
	r := &LocalRunner{Local: runner.NewLocal(options), refs: map[runner.SessionRef]struct{}{}}
	options.GracePeriod = 50 * time.Millisecond
	cleanup := runner.NewLocal(options)
	t.Cleanup(func() {
		r.mu.Lock()
		refs := make([]runner.SessionRef, 0, len(r.refs))
		for ref := range r.refs {
			refs = append(refs, ref)
		}
		r.mu.Unlock()
		sort.Slice(refs, func(i, j int) bool { return refs[i].PhaseDir < refs[j].PhaseDir })
		for _, ref := range refs {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := cleanup.StopSession(ctx, ref)
			cancel()
			if err != nil {
				t.Errorf("stop owned fixture session %s: %v", ref.Name, err)
			}
		}
	})
	return r
}

func (r *LocalRunner) remember(ref runner.SessionRef) {
	if ref.Name == "" {
		return
	}
	r.mu.Lock()
	r.refs[ref] = struct{}{}
	r.mu.Unlock()
}
func (r *LocalRunner) StartSession(ctx context.Context, req runner.SessionRequest) (runner.SessionRef, error) {
	ref, err := r.Local.StartSession(ctx, req)
	r.remember(ref)
	return ref, err
}
func (r *LocalRunner) SessionStatus(ctx context.Context, ref runner.SessionRef) (runner.SessionStatus, error) {
	status, err := r.Local.SessionStatus(ctx, ref)
	if err != nil || status.State != runner.SessionMissing {
		r.remember(ref)
	}
	return status, err
}
