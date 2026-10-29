package integration_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestAgentPortEnvironmentReachesImplementReviewAndFixWrappers(t *testing.T) {
	t.Parallel()

	script := `printf '%s\n' "$MERGEYARD_RUN_ID" "$MERGEYARD_PHASE" "$MERGEYARD_PORT_BASE" "$MERGEYARD_PORT_COUNT" "$EXPLICIT_VALUE" > "$TRACE_DIR/$MERGEYARD_PHASE"` + "\n" + testkit.LoopScript(`printf fixed > feature.txt; `+testkit.FixedReport)
	_, runtime, api, _, cfg, r := testkit.TmuxFlow(t, script)
	cfg.AgentPorts = config.AgentPorts{Base: 6100, Size: 11}
	trace := t.TempDir()
	env := map[string]string{"TRACE_DIR": trace, "EXPLICIT_VALUE": "kept", "MERGEYARD_PORT_BASE": "untrusted"}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	testkit.Finish(t, s, workflow.Active, workflow.Review)
	// Rebuild the control plane before review and fix, keeping the stored range.
	cfg.AgentPorts = config.AgentPorts{Base: 6200, Size: 10}
	s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	for _, tc := range []struct{ Phase, base, count, dir string }{
		{"implement", "6100", "5", "implement-0-1"},
		{"review", "6105", "6", "review-2-1"},
		{"fix", "6100", "5", "fix-1-1"},
	} {
		data, err := os.ReadFile(filepath.Join(trace, tc.Phase))
		want := strings.Join([]string{run.ID, tc.Phase, tc.base, tc.count, "kept", ""}, "\n")
		if err != nil || string(data) != want {
			t.Fatalf("%s wrapper environment: %q, want %q: %v", tc.Phase, data, want, err)
		}
		input, err := os.ReadFile(filepath.Join(runtime.Workspace.Root, "runs", run.ID, "phases", tc.dir, "input.md"))
		for _, name := range []string{"MERGEYARD_RUN_ID", "MERGEYARD_PHASE", "MERGEYARD_PORT_BASE", "MERGEYARD_PORT_COUNT", "AGENTS.md", "CLAUDE.md"} {
			if err != nil || !strings.Contains(string(input), name) {
				t.Fatalf("%s input missing isolation instruction %s: %v", tc.Phase, name, err)
			}
		}
	}
	if env["MERGEYARD_PORT_BASE"] != "untrusted" {
		t.Fatal("scheduler changed caller environment")
	}
}
