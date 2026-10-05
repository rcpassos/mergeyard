package scheduler_test

import (
	"context"
	"fmt"
	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/web"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

const replacementID = "01a10c61-253c-7173-a0d7-d82901ccda96"

// Arrange a saved role conversation. Review resumes in round two; fix resumes
// the implementation; implementation starts from a persisted conversation.
func recoveryFlow(t *testing.T, agent string, phase workflow.Phase, outcome string) (*scheduler.Scheduler, *app.Runtime, *fakeGitHub, string, config.Config, runner.Runner) {
	t.Helper()
	_, runtime, api, remote, cfg, r := pairingFlow(t, agent, agent, `printf fixed > feature.txt; `+fixedReport)
	round := 1
	oldID := codexID
	if phase == workflow.Review {
		round, oldID = 2, reviewerCodexID
	}
	first := fmt.Sprintf("%s-%d-1", phase, round)
	second := fmt.Sprintf("%s-%d-2", phase, round)
	if phase == workflow.Implement {
		first, second = "implement-0-1", "implement-0-2"
	}
	script := loopScript(`printf fixed > feature.txt; ` + fixedReport)
	executable := cfg.Agents.Claude.Executable
	if agent == "codex" {
		executable = cfg.Agents.Codex.Executable
		script = nativeCodexScript(script)
		injection := `case "$phase_dir" in
 *` + first + `*) echo 'Error: thread/resume: thread/resume failed: no rollout found for thread id ` + oldID + ` (code -32600)' >&2; exit 1;;
 *` + second + `*) identity='` + replacementID + `';;
 esac
 `
		if outcome == "unrelated" {
			injection = strings.Replace(injection, "Error: thread/resume: thread/resume failed: no rollout found for thread id "+oldID+" (code -32600)", "Authentication failed", 1)
		}
		if outcome == "exhausted" || outcome == "failed" {
			third := strings.TrimSuffix(second, "2") + "3"
			injection += `case "$phase_dir" in *` + third + `*) echo 'Error: thread/resume: thread/resume failed: no rollout found for thread id ` + replacementID + ` (code -32600)' >&2; exit 1;; esac
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
		if _, err := runtime.DB.Exec("UPDATE runs SET implementer_session_id=? WHERE id=?", codexID, run.ID); err != nil {
			t.Fatal(err)
		}
	}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	return s, runtime, api, remote, cfg, r
}

func TestMissingRoleSessionRecoveryAcrossPhases(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		for _, phase := range []workflow.Phase{workflow.Implement, workflow.Review, workflow.Fix} {
			t.Run(agent+"/"+string(phase), func(t *testing.T) {
				s, runtime, api, remote, cfg, _ := recoveryFlow(t, agent, phase, "success")
				state, finalPhase := workflow.WaitingForCI, workflow.Review
				if phase == workflow.Implement {
					state = workflow.Active
				}
				run := finish(t, s, state, finalPhase)
				if len(run.SessionRecoveries) != 1 || run.SessionRecoveries[0].SessionID == "" || run.SessionRecoveries[0].SessionID == run.SessionRecoveries[0].PreviousSessionID {
					t.Fatalf("lost warning or identity: %+v", run.SessionRecoveries)
				}
				round := 1
				if phase == workflow.Implement {
					round = 0
				}
				if phase == workflow.Review {
					round = 2
				}
				input, err := os.ReadFile(fmt.Sprintf("%s/runs/%s/phases/%s-%d-2/input.md", runtime.Workspace.Root, run.ID, phase, round))
				for _, value := range []string{"Implement this", "owner/repo", "#7"} {
					if err != nil || !strings.Contains(string(input), value) {
						t.Fatalf("missing %q: %s %v", value, input, err)
					}
				}
				if phase != workflow.Implement {
					for _, value := range []string{"F1", "F2", "feature.txt"} {
						if !strings.Contains(string(input), value) {
							t.Fatalf("lost findings/diff %q: %s", value, input)
						}
					}
				}
				if phase == workflow.Review && (!strings.Contains(string(input), "Changed source") || run.ReviewRound != 2 || run.Review.Attempt != 2 || !run.Review.Accepted) {
					t.Fatalf("lost prior fix or changed round: %+v", run)
				}
				if api.creations != 1 || gitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7") != map[workflow.Phase]string{workflow.Implement: "1", workflow.Review: "2", workflow.Fix: "2"}[phase] {
					t.Fatal("duplicate publication")
				}
				server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
				if err != nil {
					t.Fatal(err)
				}
				page := httptest.NewRecorder()
				server.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil))
				for _, value := range []string{"harness.session_resume_failed", "continuing once in a fresh session", run.SessionRecoveries[0].SessionID, run.SessionRecoveries[0].PreviousSessionID} {
					if page.Code != 200 || !strings.Contains(page.Body.String(), value) {
						t.Fatalf("detail/timeline missing %q: %s", value, page.Body.String())
					}
				}
			})
		}
	}
}

func TestMissingSessionFreshResultAndUnrelatedStartupRemainBounded(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		for _, phase := range []workflow.Phase{workflow.Implement, workflow.Review, workflow.Fix} {
			for _, outcome := range []string{"invalid", "unrelated", "failed"} {
				t.Run(agent+"/"+string(phase)+"/"+outcome, func(t *testing.T) {
					s, runtime, api, _, cfg, r := recoveryFlow(t, agent, phase, outcome)
					run := finish(t, s, workflow.NeedsAttention, phase)
					expected := 0
					if outcome == "invalid" || outcome == "failed" {
						expected = 1
						if outcome == "invalid" && run.LastErrorCode != "phase.result_invalid" {
							t.Fatalf("accepted invalid result: %+v", run)
						}
					}
					if outcome == "failed" && run.LastErrorCode != "phase.retries_exhausted" && run.LastErrorCode != "phase.execution_failed" {
						t.Fatalf("normal attempt budget changed: %+v", run)
					}
					if len(run.SessionRecoveries) != expected || run.ApprovedSHA != "" {
						t.Fatalf("unbounded recovery: %+v", run)
					}
					restarted, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
					if err != nil {
						t.Fatal(err)
					}
					for range 2 {
						if _, err := restarted.Reconcile(context.Background()); err != nil {
							t.Fatal(err)
						}
					}
					saved, err := runtime.Workflow.Get(context.Background(), run.ID)
					if err != nil || len(saved.SessionRecoveries) != expected || saved.State != workflow.NeedsAttention {
						t.Fatalf("restart replenished recovery: %+v %v", saved, err)
					}
				})
			}
		}
	}
}

func TestMissingSessionRecoveryAllowanceCannotBeReused(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		for _, phase := range []workflow.Phase{workflow.Implement, workflow.Review, workflow.Fix} {
			t.Run(agent+"/"+string(phase), func(t *testing.T) {
				s, runtime, api, _, cfg, r := recoveryFlow(t, agent, phase, "exhausted")
				run := finish(t, s, workflow.NeedsAttention, phase)
				if len(run.SessionRecoveries) != 1 || run.LastErrorCode != "harness.session_resume_failed" {
					t.Fatalf("allowance replenished: %+v", run)
				}
				restarted, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := restarted.Reconcile(context.Background()); err != nil {
					t.Fatal(err)
				}
				var attempts int
				if err := runtime.DB.QueryRow("SELECT count(*) FROM phase_attempts WHERE run_id=? AND phase=? AND round=?", run.ID, phase, map[workflow.Phase]int{workflow.Implement: 0, workflow.Fix: 1, workflow.Review: 2}[phase]).Scan(&attempts); err != nil {
					t.Fatal(err)
				}
				if attempts != 3 {
					t.Fatalf("second recovery launched: %d", attempts)
				}
			})
		}
	}
}
