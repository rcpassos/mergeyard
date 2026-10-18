package scheduler

import "github.com/rcpassos/mergeyard/internal/execution/durable"

func (s *Scheduler) execution() durable.Executor {
	return durable.Executor{DB: s.db, Workflow: s.workflow, Events: s.bus, Runner: s.deps.Runner, Harnesses: s.harnesses, Now: s.deps.Now}
}
