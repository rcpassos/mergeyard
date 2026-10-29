package testkit

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

const ReviewerCodexID = "01a10c5c-8a43-7c52-ba89-5a6d6ec1e22c"

// Adapt the existing Claude fixture reports into native Codex artifacts. The
// fixture insists on the phase schema and complete input on every invocation.
func NativeCodexScript(script string) string {
	return `last_message=''
 schema=''
 previous=''
 for arg do
  if [ "$previous" = '-o' ]; then last_message=$arg; fi
  if [ "$previous" = '--output-schema' ]; then schema=$arg; fi
  previous=$arg
 done
 phase_dir=${schema%/schema.json}
 test -s "$schema" && test -s "$phase_dir/input.md" || exit 9
 /usr/bin/grep -q 'Implement this' "$phase_dir/input.md" || exit 10
 identity='` + CodexID + `'
 case "$phase_dir" in *review-*) identity='` + ReviewerCodexID + `'; /usr/bin/grep -q 'findings' "$schema" || exit 11;; *fix-*) /usr/bin/grep -q 'responses' "$schema" || exit 12;; esac
 expected_model=implement-model
 expected_effort=medium
 expected_skill=implement
 case "$phase_dir" in *review-*) expected_model=review-model; expected_effort=low; expected_skill=review;; esac
 case "$*" in *"--model $expected_model "*) ;; *) exit 15;; esac
 case "$*" in *"model_reasoning_effort=\"$expected_effort\""*) ;; *) exit 16;; esac
 case "$*" in *"\$$expected_skill Read "*) ;; *) exit 17;; esac
 case "$*" in *'-s workspace-write -c approval_policy="never" -c sandbox_workspace_write.network_access=true'*) ;; *) exit 18;; esac
 case "$phase_dir" in *fix-*|*review-2-*) case "$*" in *"resume -- $identity "*) ;; *) exit 14;; esac;; esac
 case "$*" in *'resume -- '*) case "$*" in *"resume -- $identity "*) ;; *) exit 13;; esac;; esac
 printf '%s\n' "{\"type\":\"thread.started\",\"thread_id\":\"$identity\"}"
 (
 ` + script + `
 ) > "$phase_dir/fixture-report"
 status=$?
 if [ "$status" != 0 ]; then exit "$status"; fi
 # Fixture output is one compact result record with the structured object last.
 sed 's/^.*"structured_output"://; s/}$//' "$phase_dir/fixture-report" > "$last_message"
 printf '%s\n' '{"type":"turn.completed"}'
 `
}

func TmuxPairingFlow(t *testing.T, implementer, reviewer, fix string) (*scheduler.Scheduler, *app.Runtime, *FakeGitHub, string, config.Config, runner.Runner) {
	t.Helper()
	script := LoopScript(fix)
	_, runtime, api, remote, cfg, r := TmuxFlow(t, script)
	executable := filepath.Join(t.TempDir(), "fake-codex")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+NativeCodexScript(script)), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Agents.Codex.Executable = executable
	cfg.Repositories[0].Implementer = config.Role{Agent: implementer, Model: "implement-model", Effort: "medium", Skills: []string{"implement"}, MaxAttempts: 1}
	cfg.Repositories[0].Reviewer = config.Role{Agent: reviewer, Model: "review-model", Effort: "low", Skills: []string{"review"}, MaxAttempts: 1}
	s, err := TestScheduler(cfg, SchedulerResources(runtime), scheduler.Dependencies{GitHub: BranchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	return s, runtime, api, remote, cfg, r
}

func ReplacePhaseScript(t *testing.T, cfg config.Config, phase workflow.Phase, script string) {
	t.Helper()
	executable, agent := cfg.Agents.Claude.Executable, cfg.Repositories[0].Reviewer.Agent
	if phase == workflow.Implement || phase == workflow.Fix {
		agent = cfg.Repositories[0].Implementer.Agent
	}
	if agent == "codex" {
		executable, script = cfg.Agents.Codex.Executable, NativeCodexScript(script)
	}
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
}

func WaitForPhase(t *testing.T, s *scheduler.Scheduler, phase workflow.Phase) workflow.Run {
	t.Helper()
	var result workflow.Run
	// This drives implementation and possibly review before reaching the held
	// phase; use the workflow deadline rather than the short artifact deadline.
	WaitForWithin(t, 30*time.Second, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != 1 || runs[0].Phase != phase {
			return false
		}
		result = runs[0]
		if phase == workflow.Implement {
			return result.Implementer != nil && result.Implementer.Status == "running"
		}
		if phase == workflow.Review {
			return result.Review != nil && result.Review.Status == "running" && result.Review.SessionID != ""
		}
		return result.Fix != nil && result.Fix.Status == "running"
	})
	return result
}

type FailReviewLaunch struct {
	runner.Runner
	Observe func()
}

func (r FailReviewLaunch) StartSession(ctx context.Context, req runner.SessionRequest) (runner.SessionRef, error) {
	if req.Phase == "review" {
		r.Observe()
		return runner.SessionRef{}, &fault.Error{Code: "phase.launch_failed", Message: "fixture executable unavailable"}
	}
	return r.Runner.StartSession(ctx, req)
}
