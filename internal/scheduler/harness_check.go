package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/sessions"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// CheckHarness reserves the same durable gate as selected-run Retry and launches
// only a minimal request. The scheduler observes it on ticks, including restart.
func (s *Scheduler) CheckHarness(ctx context.Context, name string) (workflow.HarnessCheck, error) {
	s.tick.Lock()
	defer s.tick.Unlock()
	adapter, ok := s.harnesses[name]
	if !ok || (name != "claude" && name != "codex") {
		return workflow.HarnessCheck{}, &fault.Error{Code: "harness.unknown", Message: "Choose claude or codex"}
	}
	if !adapter.Capabilities().CreditExhaustionDetection {
		return workflow.HarnessCheck{}, &fault.Error{Code: "harness.credit_detection_unavailable", Message: "Credit recovery is unavailable until this harness supports exhausted-credit detection"}
	}
	if err := s.reconcileHarnessChecks(ctx); err != nil {
		return workflow.HarnessCheck{}, err
	}
	if err := s.workflow.ReconcileCreditProbes(ctx); err != nil {
		return workflow.HarnessCheck{}, err
	}
	limit, err := workflow.LoadHarnessLimit(ctx, s.db, name)
	if err != nil {
		return workflow.HarnessCheck{}, err
	}
	if limit == nil || limit.Reason != "credits_exhausted" {
		return workflow.HarnessCheck{}, &fault.Error{Code: "harness.probe_unavailable", Message: "Check availability is only available for an exhausted-credit block"}
	}
	id := uuid.NewString()
	dir := filepath.Join(s.workspace.Root, "harness-checks", id)
	command, err := adapter.BuildCheckInvocation(filepath.Join(dir, "empty"), nil)
	if err != nil {
		return workflow.HarnessCheck{}, err
	}
	req := runner.SessionRequest{RunID: id, Phase: "harness-check", Attempt: 1, PhaseDir: dir, Command: command}
	data, err := json.Marshal(req)
	if err != nil {
		return workflow.HarnessCheck{}, err
	}
	c := workflow.HarnessCheck{ID: id, Harness: name, RestrictionID: limit.RestrictionID, RequestJSON: string(data), ProcessSession: sessions.Name(req), PhaseDir: dir, Status: "reserved"}
	if err := s.workflow.ReserveHarnessCheck(ctx, c, s.deps.Now().UTC()); err != nil {
		return workflow.HarnessCheck{}, err
	}
	if err := s.launchHarnessCheck(ctx, c); err != nil {
		return c, err
	}
	checks, err := workflow.LoadHarnessChecks(ctx, s.db, name, true)
	if err != nil {
		return c, err
	}
	for _, check := range checks {
		if check.ID == id {
			return check, nil
		}
	}
	return c, nil
}

func (s *Scheduler) launchHarnessCheck(ctx context.Context, c workflow.HarnessCheck) error {
	var req runner.SessionRequest
	if err := json.Unmarshal([]byte(c.RequestJSON), &req); err != nil {
		return err
	}
	if err := os.MkdirAll(req.Command.Dir, 0700); err != nil {
		return err
	}
	// Claim physical execution before the external launch. An ambiguous startup
	// never replays a model request; restart observes its journal or releases it.
	if !s.harnesses[c.Harness].Capabilities().CreditExhaustionDetection {
		return s.workflow.CompleteHarnessCheck(ctx, c, s.deps.Now().UTC(), false, "Exhausted-credit detection is unavailable; update the harness support before checking again.", nil)
	}
	if err := s.workflow.StartHarnessCheck(ctx, c, s.deps.Now().UTC()); err != nil {
		var failure *fault.Error
		if !errors.As(err, &failure) || failure.Code != "harness.probe_unavailable" {
			return err
		}
		return s.workflow.CompleteHarnessCheck(ctx, c, s.deps.Now().UTC(), false, "Restriction changed before launch; refresh status and check again.", nil)
	}
	req.Command.Env = s.deps.Env
	ref, err := s.deps.Runner.StartSession(ctx, req)
	if err != nil && ref.Name == "" {
		recordErr := s.workflow.CompleteHarnessCheck(context.WithoutCancel(ctx), c, s.deps.Now().UTC(), false, "Could not start the availability request; check the harness installation and try again.", nil)
		return errors.Join(err, recordErr)
	}
	return err
}

func (s *Scheduler) reconcileHarnessChecks(ctx context.Context) error {
	checks, err := workflow.LoadHarnessChecks(ctx, s.db, "", true)
	if err != nil {
		return err
	}
	for _, c := range checks {
		if c.Status == "reserved" {
			if err := s.launchHarnessCheck(ctx, c); err != nil {
				return err
			}
			continue
		}
		status, err := s.deps.Runner.SessionStatus(ctx, runner.SessionRef{Name: c.ProcessSession, PhaseDir: c.PhaseDir})
		if err != nil {
			return err
		}
		if status.State == runner.SessionRunning {
			continue
		}
		proven := false
		message := "Availability was not proven; check login, credits, and captured harness output, then check again."
		var wait *workflow.HarnessWait
		if status.State == runner.SessionExited && status.ExitCode != nil {
			stdout, err := s.deps.Runner.ReadFile(ctx, filepath.Join(c.PhaseDir, "events.jsonl"))
			if err != nil {
				return err
			}
			stderr, err := s.deps.Runner.ReadFile(ctx, filepath.Join(c.PhaseDir, "stderr.log"))
			if err != nil {
				return err
			}
			artifacts := harness.PhaseArtifacts{Stdout: stdout, Stderr: stderr, ExitCode: *status.ExitCode}
			adapter := s.harnesses[c.Harness]
			proven = adapter.NativeSucceeded(artifacts)
			if proven {
				message = "Model response completed; account availability was proven."
			} else {
				outcome := adapter.ClassifyFailure(artifacts, s.deps.Now().UTC())
				now := s.deps.Now().UTC()
				switch outcome.Kind {
				case harness.CreditsExhausted:
					wait = &workflow.HarnessWait{Harness: c.Harness, Reason: "credits_exhausted", DetectedAt: now, Source: outcome.Source}
					message = "Credits are still exhausted; restore account credits and check again."
				case harness.TemporaryLimit:
					reset, source := now.Add(s.cfg.UsageLimits.Cooldown), "default_cooldown"
					if outcome.ResetAt.After(now) {
						reset, source = outcome.ResetAt.UTC(), "reported"
					}
					wait = &workflow.HarnessWait{Harness: c.Harness, Reason: "temporary_limit", DetectedAt: now, ResetAt: reset, ResetTimeSource: source, Source: outcome.Source}
					message = "Temporary usage limit; wait for the reset before checking availability again."
				}
			}
		}
		if err := s.workflow.CompleteHarnessCheck(ctx, c, s.deps.Now().UTC(), proven, message, wait); err != nil {
			return err
		}
	}
	return nil
}
