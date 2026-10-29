package testkit

import (
	"testing"

	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/sessions"
)

type ScenarioCrash struct{}

// crashTick interrupts the boundary before its caller can acknowledge it in
// SQLite. Only the controller is discarded; external refs and sessions survive.
func CrashTick(t *testing.T, s *scheduler.Scheduler) (crashed bool) {
	t.Helper()
	defer func() {
		if value := recover(); value != nil {
			if _, ok := value.(ScenarioCrash); !ok {
				panic(value)
			}
			crashed = true
		}
	}()
	if err := s.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	return false
}

func RunnerRef(req runner.SessionRequest) runner.SessionRef {
	return runner.SessionRef{Name: sessions.Name(req), PhaseDir: req.PhaseDir}
}
