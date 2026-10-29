package integration_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestIssueToDraftPRAndRepeatedTicksAfterRestart(t *testing.T) {
	t.Parallel()

	s, runtime, api, remote, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	run := testkit.Finish(t, s, workflow.Active, workflow.Review)
	if run.PRNumber != 101 || run.ReviewRound != 1 {
		t.Fatalf("M1 endpoint metadata: %+v", run)
	}
	if api.Creations != 1 || !api.Prs["mergeyard/issue-7"].Draft || !strings.Contains(api.Prs["mergeyard/issue-7"].Body, "Closes #7") {
		t.Fatal("expected one draft PR with closing reference")
	}
	if got := testkit.GitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7"); got != "1" {
		t.Fatalf("implementation commits = %s", got)
	}
	if got := testkit.GitCommand(t, remote, "show", "mergeyard/issue-7:feature.txt"); got != "implemented" {
		t.Fatalf("pushed contents = %s", got)
	}
	issue := api.Issues["owner/repo"][0]
	if !testkit.Has(issue, "agent-running") || testkit.Has(issue, "ready-for-agent") {
		t.Fatalf("claim labels = %+v", issue.Labels)
	}
	history, err := runtime.Events.History(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var transitions []string
	for _, e := range history {
		if e.Type == "run.claimed" || e.Type == "run.preparing" || e.Type == "phase.started" {
			transitions = append(transitions, e.Type)
		}
	}
	if strings.Join(transitions, ",") != "run.claimed,run.preparing,phase.started" {
		t.Fatalf("lifecycle = %v", transitions)
	}
	// Simulate an accidental ready label and a fresh control plane using the same DB.
	api.Issues["owner/repo"][0].Labels = []github.Label{{Name: "ready-for-agent"}}
	restarted, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err := restarted.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := restarted.Runs(context.Background())
	if err != nil || len(runs) != 1 || runs[0].ID != run.ID || api.Creations != 1 {
		t.Fatalf("duplicate run/PR after restart: %+v, %v, PRs=%d", runs, err, api.Creations)
	}
	treePaths, _ := filepath.Glob(filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", "*"))
	if len(treePaths) != 1 {
		t.Fatalf("worktrees = %v", treePaths)
	}
	phasePaths, _ := filepath.Glob(filepath.Join(runtime.Workspace.Root, "runs", run.ID, "phases", "*"))
	if len(phasePaths) != 2 {
		t.Fatalf("expected implement and first review attempts: %v", phasePaths)
	}
	input, err := os.ReadFile(filepath.Join(phasePaths[0], "input.md"))
	if err != nil || !strings.Contains(string(input), "Implement this") {
		t.Fatalf("phase input missing: %s, %v", input, err)
	}
	result, err := os.ReadFile(filepath.Join(phasePaths[0], "result.json"))
	var parsed map[string]any
	if err != nil || json.Unmarshal(result, &parsed) != nil || parsed["status"] != "success" {
		t.Fatalf("parsed phase result missing: %s, %v", result, err)
	}
}

func TestImplementFailuresPreserveWorkspaceAndRequireAttention(t *testing.T) {
	t.Parallel()

	cases := []struct{ name, Script, Code string }{
		{"blocked", `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":4,"verification":[],"criteria":[],"human_action":{"decision":"Clarify the required behavior","files":[],"options":[]},"status":"blocked","summary":"Need a product decision"}}'`, "phase.blocked"},
		{"invalid", `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"status":"success"}}'`, "phase.result_invalid"},
		{"failed", `exit 1`, "phase.retries_exhausted"},
		{"no-changes", `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":4,"verification":[],"criteria":[],"human_action":null,"status":"success","summary":"Nothing to do"}}'`, "git.no_changes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s, runtime, api, remote, _, _ := testkit.TmuxFlow(t, tc.Script)
			run := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
			if run.LastErrorCode != tc.Code || run.LastErrorMessage == "" {
				t.Fatalf("failure = %+v", run)
			}
			issue := api.Issues["owner/repo"][0]
			if testkit.Has(issue, "agent-running") || testkit.Has(issue, "ready-for-agent") || !testkit.Has(issue, "agent-needs-attention") {
				t.Fatalf("attention labels = %+v", issue.Labels)
			}
			trees, _ := filepath.Glob(filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", "*"))
			if len(trees) != 1 {
				t.Fatal("attention discarded worktree")
			}
			if api.Creations != 0 || testkit.GitCommand(t, remote, "branch", "--list", "mergeyard/issue-7") != "" {
				t.Fatal("failed implementation published work")
			}
			for range 2 {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			phases, _ := filepath.Glob(filepath.Join(runtime.Workspace.Root, "runs", run.ID, "phases", "*"))
			if len(phases) != 1 {
				t.Fatal("attention retried without user action")
			}
		})
	}
}

func TestPhaseRetryIsBoundedAndKeepsOneRun(t *testing.T) {
	t.Parallel()

	for _, success := range []bool{true, false} {
		t.Run(map[bool]string{true: "succeeds", false: "exhausted"}[success], func(t *testing.T) {
			t.Parallel()

			script := `if [ ! -f retry-marker ]; then
 printf '%s\n' 'first attempt' > retry-marker
 printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":4,"verification":[],"criteria":[],"human_action":null,"status":"failed","summary":"Temporary failure"}}'
 else
 ` + testkit.SuccessfulScript + `
 fi`
			if !success {
				script = `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":4,"verification":[],"criteria":[],"human_action":null,"status":"failed","summary":"Persistent failure"}}'`
			}
			_, runtime, api, _, cfg, r := testkit.TmuxFlow(t, script)
			cfg.Repositories[0].Implementer.MaxAttempts = 2
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			state, phase := workflow.Active, workflow.Review
			if !success {
				state, phase = workflow.NeedsAttention, workflow.Implement
			}
			run := testkit.Finish(t, s, state, phase)
			for range 3 {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			history, err := runtime.Events.History(context.Background(), 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			var attempts []int
			for _, event := range history {
				if event.RunID == run.ID && event.Type == "phase.attempt_started" && strings.Contains(string(event.Payload), `"phase":"implement"`) {
					var payload struct {
						Attempt int `json:"attempt"`
					}
					if err := json.Unmarshal(event.Payload, &payload); err != nil {
						t.Fatal(err)
					}
					attempts = append(attempts, payload.Attempt)
				}
			}
			if len(attempts) != 2 || attempts[0] != 1 || attempts[1] != 2 {
				t.Fatalf("attempt-start notifications = %v; want [1 2]", attempts)
			}
			phases, _ := filepath.Glob(filepath.Join(runtime.Workspace.Root, "runs", run.ID, "phases", "*"))
			want := 2
			if success {
				want = 3
			}
			if len(phases) != want {
				t.Fatalf("phase attempts = %v", phases)
			}
			runs, _ := s.Runs(context.Background())
			if len(runs) != 1 {
				t.Fatal("retry created another run")
			}
		})
	}
}

func TestSchedulerRestartObservesExistingTmuxAttempt(t *testing.T) {
	t.Parallel()

	script := `while [ ! -f release ]; do /bin/sleep 0.02; done
 ` + testkit.SuccessfulScript
	s, runtime, api, _, cfg, r := testkit.TmuxFlow(t, script)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(context.Background())
	if len(runs) != 1 || runs[0].Phase != workflow.Implement {
		t.Fatalf("running attempt = %+v", runs)
	}
	restarted, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := restarted.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	paths, _ := filepath.Glob(filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", "*"))
	if len(paths) != 1 {
		t.Fatal("duplicate worktree after restart")
	}
	if err := os.WriteFile(filepath.Join(paths[0], "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, restarted, workflow.Active, workflow.Review)
	phases, _ := filepath.Glob(filepath.Join(runtime.Workspace.Root, "runs", run.ID, "phases", "*"))
	if len(phases) != 1 {
		t.Fatalf("restart duplicated running process: %v", phases)
	}
}
