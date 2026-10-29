package integration_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestRepositoryExclusionsReachStagingAndPhaseDiagnostics(t *testing.T) {
	t.Parallel()

	for _, checks := range []bool{false, true} {
		t.Run(map[bool]string{false: "without checks", true: "with checks"}[checks], func(t *testing.T) {
			t.Parallel()

			script := `mkdir -p .venv/bin
printf tool > .venv/bin/python
printf tracked > README.md
` + testkit.SuccessfulScript
			_, rt, api, remote, cfg, r := testkit.TmuxFlow(t, script)
			cfg.Repositories[0].Exclude = []string{".venv/", "README.md"}
			if checks {
				cfg.Repositories[0].Checks = []string{"test -f .venv/bin/python && test -f feature.txt"}
			}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			run := testkit.Finish(t, s, workflow.Active, workflow.Review)
			if testkit.GitCommand(t, remote, "ls-tree", "-r", "--name-only", "mergeyard/issue-7", ".venv") != "" {
				t.Fatal("excluded setup output published")
			}
			if testkit.GitCommand(t, remote, "show", "mergeyard/issue-7:README.md") != "tracked" {
				t.Fatal("excluded tracked change not published")
			}
			path := filepath.Join(rt.Workspace.Root, "runs", run.ID, "phases", "implement-0-1", "staging.json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var diagnostics []managedgit.Diagnostic
			if err := json.Unmarshal(data, &diagnostics); err != nil {
				t.Fatal(err)
			}
			if len(diagnostics) != 2 || !strings.Contains(string(data), "git.exclude_tracked") || !strings.Contains(string(data), ".venv/bin/python") {
				t.Fatalf("phase diagnostics: %s", data)
			}
			history, err := rt.Events.History(context.Background(), 0, 1000)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, e := range history {
				if e.Type == "git.staging_diagnostics" && strings.Contains(string(e.Payload), "git.exclude_tracked") {
					found = true
				}
			}
			if !found {
				t.Fatal("staging warning not recorded in run events")
			}
			// Reloading the run's persisted Git ownership must still allow leftovers.
			s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
		})
	}
}

func TestExcludedOnlyPhaseRetainsDiagnosticsOnNoChanges(t *testing.T) {
	t.Parallel()

	// Reuse the shared result schema, then leave only excluded changes.
	script := testkit.SuccessfulScript + `
rm feature.txt
mkdir -p .venv/bin
printf tool > .venv/bin/python
`
	_, rt, api, remote, cfg, r := testkit.TmuxFlow(t, script)
	cfg.Repositories[0].Exclude = []string{".venv/"}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
	if run.LastErrorCode != "git.no_changes" || api.Creations != 0 || testkit.GitCommand(t, remote, "branch", "--list", "mergeyard/issue-7") != "" {
		t.Fatalf("excluded-only phase published: %+v", run)
	}
	path := filepath.Join(rt.Workspace.Root, "runs", run.ID, "phases", "implement-0-1", "staging.json")
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), ".venv/bin/python") {
		t.Fatalf("no-changes diagnostics lost: %s %v", data, err)
	}
}
