package scheduler

import (
	"context"
	"errors"
	"strconv"

	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func agentPortsExhausted(err error) bool {
	var coded *fault.Error
	return errors.As(err, &coded) && coded.Code == "agent_ports.exhausted"
}

// phaseEnvironment extends the explicit environment; caller maps are never
// mutated and reserved isolation variables cannot be overridden by them.
func (s *Scheduler) phaseEnvironment(ctx context.Context, id string, phase workflow.Phase) (map[string]string, error) {
	ports, err := s.workflow.EnsureAgentPorts(ctx, id, s.cfg.AgentPorts)
	if err != nil {
		return nil, err
	}
	base, count := ports.Base, ports.Size/2
	if phase == workflow.Review {
		base += count
		count = ports.Size - count
	}
	env := make(map[string]string, len(s.deps.Env)+4)
	for key, value := range s.deps.Env {
		env[key] = value
	}
	env["MERGEYARD_RUN_ID"] = id
	env["MERGEYARD_PHASE"] = string(phase)
	env["MERGEYARD_PORT_BASE"] = strconv.Itoa(base)
	env["MERGEYARD_PORT_COUNT"] = strconv.Itoa(count)
	return env, nil
}
