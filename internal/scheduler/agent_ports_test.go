package scheduler_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestAgentPortEnvironmentReachesImplementReviewAndFixWrappers(t *testing.T) {
	script := `printf '%s\n' "$MERGEYARD_RUN_ID" "$MERGEYARD_PHASE" "$MERGEYARD_PORT_BASE" "$MERGEYARD_PORT_COUNT" "$EXPLICIT_VALUE" > "$TRACE_DIR/$MERGEYARD_PHASE"` + "\n" + loopScript(`printf fixed > feature.txt; `+fixedReport)
	_, runtime, api, _, cfg, r := localFlow(t, script)
	cfg.AgentPorts = config.AgentPorts{Base: 6100, Size: 11}
	trace := t.TempDir()
	env := map[string]string{"TRACE_DIR": trace, "EXPLICIT_VALUE": "kept", "MERGEYARD_PORT_BASE": "untrusted"}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	finish(t, s, workflow.Active, workflow.Review)
	// Rebuild the control plane before review and fix, keeping the stored range.
	cfg.AgentPorts = config.AgentPorts{Base: 6200, Size: 10}
	s, err = scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
	for _, tc := range []struct{ phase, base, count, dir string }{
		{"implement", "6100", "5", "implement-0-1"},
		{"review", "6105", "6", "review-2-1"},
		{"fix", "6100", "5", "fix-1-1"},
	} {
		data, err := os.ReadFile(filepath.Join(trace, tc.phase))
		want := strings.Join([]string{run.ID, tc.phase, tc.base, tc.count, "kept", ""}, "\n")
		if err != nil || string(data) != want {
			t.Fatalf("%s wrapper environment: %q, want %q: %v", tc.phase, data, want, err)
		}
		input, err := os.ReadFile(filepath.Join(runtime.Workspace.Root, "runs", run.ID, "phases", tc.dir, "input.md"))
		for _, name := range []string{"MERGEYARD_RUN_ID", "MERGEYARD_PHASE", "MERGEYARD_PORT_BASE", "MERGEYARD_PORT_COUNT", "AGENTS.md", "CLAUDE.md"} {
			if err != nil || !strings.Contains(string(input), name) {
				t.Fatalf("%s input missing isolation instruction %s: %v", tc.phase, name, err)
			}
		}
	}
	if env["MERGEYARD_PORT_BASE"] != "untrusted" {
		t.Fatal("scheduler changed caller environment")
	}
}
