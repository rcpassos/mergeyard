package integration_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestMissingCIFixSessionRecoveryRetainsCompleteDiagnostics(t *testing.T) {
	t.Parallel()

	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			t.Parallel()

			_, runtime, base, _, cfg, r := testkit.TmuxPairingFlow(t, agent, agent, testkit.CiFixedReport)
			script := testkit.CiFixScript(`printf repaired > feature.txt; ` + testkit.CiFixedReport)
			executable := cfg.Agents.Claude.Executable
			if agent == "claude" {
				script = `case "$*" in *fix-1-1*) printf 'No conversation found with session ID: %s\n' "$3" >&2; exit 1;; esac
` + script
			} else {
				executable = cfg.Agents.Codex.Executable
				script = testkit.NativeCodexScript(script)
				injection := `case "$phase_dir" in
 *fix-1-1*) echo 'Error: thread/resume: thread/resume failed: no rollout found for thread id ` + testkit.CodexID + ` (code -32600)' >&2; exit 1;;
 *fix-1-2*) identity='` + testkit.ReplacementID + `';;
 esac
`
				script = strings.Replace(script, ` case "$phase_dir" in *fix-*|*review-2-*)`, injection+` case "$phase_dir" in *fix-1-1*|*review-2-*)`, 1)
			}
			if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+script), 0700); err != nil {
				t.Fatal(err)
			}
			api := &testkit.CiGitHub{FakeGitHub: base, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "failure", URL: "https://checks.example/build"}}}}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.CiBranchGitHub{CiGitHub: api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
			testkit.Finish(t, s, workflow.Active, workflow.Fix)
			api.Evidence = ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "success"}}}
			run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
			if len(run.SessionRecoveries) != 1 || len(run.FixHistory) != 2 || run.Fix.Attempt != 2 || run.ReviewRound != 2 || run.Fix.CI == nil || run.Fix.CI.RepairCause != "ci.check_failed" || run.FixHistory[0].CI == nil || run.Fix.CI.SHA != run.FixHistory[0].CI.SHA {
				t.Fatalf("lost CI recovery context: %+v", run)
			}
			input, err := os.ReadFile(fmt.Sprintf("%s/runs/%s/phases/fix-1-2/input.md", runtime.Workspace.Root, run.ID))
			for _, text := range []string{"Implement this", "CI repair", "ci.check_failed", "build", "https://checks.example/build", run.Fix.CI.SHA, "responses: []"} {
				if err != nil || !strings.Contains(string(input), text) {
					t.Fatalf("fresh CI fix missing %q: %s %v", text, input, err)
				}
			}
		})
	}
}
