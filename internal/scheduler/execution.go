package scheduler

import "github.com/rcpassos/mergeyard/internal/execution/durable"

func (s *Scheduler) execution() durable.Executor {
	return durable.Executor{DB: s.db, Workflow: s.workflow, Events: s.bus, Runner: s.deps.Runner, Harnesses: s.harnesses, Now: s.deps.Now}
}

func durableAttempt(a attempt) durable.Attempt {
	return durable.Attempt{ID: a.id, Agent: a.agent, Number: a.number, Status: a.status, Ref: a.ref, SessionID: a.sessionID, Resumed: a.resumed, Failure: a.failure, CheckRetryOf: a.checkRetryOf}
}

func schedulerAttempt(a durable.Attempt) attempt {
	return attempt{id: a.ID, agent: a.Agent, number: a.Number, status: a.Status, ref: a.Ref, sessionID: a.SessionID, resumed: a.Resumed, failure: a.Failure, checkRetryOf: a.CheckRetryOf}
}
