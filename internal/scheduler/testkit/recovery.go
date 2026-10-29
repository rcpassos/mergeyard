package testkit

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

const ReplacementID = "01a10c61-253c-7173-a0d7-d82901ccda96"

// Arrange a saved role conversation. Review resumes in round two; fix resumes
// the implementation; implementation starts from a persisted conversation.
func RecoveryFlow(t *testing.T, agent string, phase workflow.Phase, outcome string) (*scheduler.Scheduler, *app.Runtime, *FakeGitHub, string, config.Config, runner.Runner) {
	t.Helper()
	_, runtime, api, remote, cfg, r := TmuxPairingFlow(t, agent, agent, `printf fixed > feature.txt; `+FixedReport)
	round := 1
	oldID := CodexID
	if phase == workflow.Review {
		round, oldID = 2, ReviewerCodexID
	}
	first := fmt.Sprintf("%s-%d-1", phase, round)
	second := fmt.Sprintf("%s-%d-2", phase, round)
	if phase == workflow.Implement {
		first, second = "implement-0-1", "implement-0-2"
	}
	script := LoopScript(`printf fixed > feature.txt; ` + FixedReport)
	executable := cfg.Agents.Claude.Executable
	if agent == "codex" {
		executable = cfg.Agents.Codex.Executable
		script = NativeCodexScript(script)
		injection := `case "$phase_dir" in
 *` + first + `*) echo 'Error: thread/resume: thread/resume failed: no rollout found for thread id ` + oldID + ` (code -32600)' >&2; exit 1;;
 *` + second + `*) identity='` + ReplacementID + `';;
 esac
 `
		if outcome == "unrelated" {
			injection = strings.Replace(injection, "Error: thread/resume: thread/resume failed: no rollout found for thread id "+oldID+" (code -32600)", "Authentication failed", 1)
		}
		if outcome == "exhausted" || outcome == "failed" {
			third := strings.TrimSuffix(second, "2") + "3"
			injection += `case "$phase_dir" in *` + third + `*) echo 'Error: thread/resume: thread/resume failed: no rollout found for thread id ` + ReplacementID + ` (code -32600)' >&2; exit 1;; esac
 `
			script = strings.Replace(script, ` printf '%s\n' "{\"type\":\"thread.started\",\"thread_id\":\"$identity\"}"`, ` printf '%s\n' "{\"type\":\"thread.started\",\"thread_id\":\"$identity\"}"
 case "$phase_dir" in *`+second+`*) echo 'temporary execution failure' >&2; exit 1;; esac`, 1)
		}
		script = strings.Replace(script, ` case "$phase_dir" in *fix-*|*review-2-*)`, injection+` case "$phase_dir" in `+map[workflow.Phase]string{workflow.Fix: "*fix-1-1*|*review-2-*", workflow.Review: "*fix-*|*review-2-1*", workflow.Implement: "*fix-*|*review-2-*"}[phase]+`)`, 1)
		if outcome == "invalid" {
			script = strings.Replace(script, ` # Fixture output`, ` case "$phase_dir" in *`+second+`*) printf '%s\n' '{"status":"invalid"}' > "$last_message"; printf '%s\n' '{"type":"turn.completed"}'; exit 0;; esac
 # Fixture output`, 1)
		}
	} else {
		diagnostic := `printf 'No conversation found with session ID: %s\n' "$3" >&2`
		if outcome == "unrelated" {
			diagnostic = `echo 'Authentication failed' >&2`
		}
		prefix := `case "$*" in *` + first + `*) ` + diagnostic + `; exit 1;; esac
 `
		if outcome == "invalid" {
			prefix += `case "$*" in *` + second + `*) printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"status":"invalid"}}'; exit 0;; esac
 `
		}
		if outcome == "exhausted" || outcome == "failed" {
			third := strings.TrimSuffix(second, "2") + "3"
			prefix += `case "$*" in *` + second + `*) echo 'temporary execution failure' >&2; exit 1;; *` + third + `*) printf 'No conversation found with session ID: %s\n' "$3" >&2; exit 1;; esac
 `
		}
		script = prefix + script
	}
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	if outcome == "exhausted" {
		cfg.Repositories[0].Implementer.MaxAttempts = 3
		cfg.Repositories[0].Reviewer.MaxAttempts = 3
	}
	if phase == workflow.Implement {
		run, err := runtime.Workflow.Transition(context.Background(), "saved-implement", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.DB.Exec("UPDATE runs SET implementer_session_id=? WHERE id=?", CodexID, run.ID); err != nil {
			t.Fatal(err)
		}
	}
	s, err := TestScheduler(cfg, SchedulerResources(runtime), scheduler.Dependencies{GitHub: BranchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	return s, runtime, api, remote, cfg, r
}
