package scheduler_test

import (
	"context"
	"errors"
	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSetupFailureNeverStartsImplementer(t *testing.T) {
	_, rt, api, _, cfg, r := localFlow(t, successfulScript)
	cfg.Repositories[0].Setup = []string{"printf 'dependency install failed\\n' >&2; exit 7"}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.NeedsAttention, workflow.Implement)
	if run.LastErrorCode != "workspace.setup_failed" || !strings.Contains(run.LastErrorMessage, "dependency install failed") {
		t.Fatalf("setup diagnostic = %+v", run)
	}
	if run.Implementer != nil || api.creations != 0 {
		t.Fatalf("agent started after setup failure: %+v", run)
	}
}

func TestSetupRestartObservesLiveFinishedAndUnknownOutcomes(t *testing.T) {
	for _, outcome := range []string{"live", "finished", "unknown"} {
		t.Run(outcome, func(t *testing.T) {
			_, rt, api, _, cfg, r := localFlow(t, "[ -f setup-ready ] || exit 9\n"+successfulScript)
			cfg.Repositories[0].Setup = []string{"printf 'setup started\\n'; while [ ! -f release-setup ]; do /bin/sleep 0.02; done; printf ready > setup-ready"}
			s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			runs, err := s.Runs(context.Background())
			if err != nil || len(runs) != 1 {
				t.Fatalf("runs=%+v err=%v", runs, err)
			}
			run := runs[0]
			if len(run.SystemSteps) != 1 || run.SystemSteps[0].Step != "setup" || run.Implementer != nil {
				t.Fatalf("setup phase metadata=%+v", run)
			}
			ref, err := s.Watch(context.Background(), run.ID)
			if err != nil || !strings.Contains(ref.Name, "-setup-0-1") {
				t.Fatalf("watch=%+v err=%v", ref, err)
			}
			tree := setupWorktree(t, rt, run.ID)
			if outcome != "live" {
				if outcome == "unknown" {
					if err := r.StopSession(context.Background(), ref); err != nil {
						t.Fatal(err)
					}
					if err := r.RemovePath(context.Background(), filepath.Join(ref.PhaseDir, "exit.json")); err != nil {
						t.Fatal(err)
					}
				}
				if err := r.WriteFile(context.Background(), filepath.Join(tree, "release-setup"), nil, 0600); err != nil {
					t.Fatal(err)
				}
				if outcome == "finished" {
					awaitSetupSession(t, r, ref)
				}
			}
			root := rt.Workspace.Root
			if err := rt.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := app.Open(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { reopened.Close() })
			restarted, err := scheduler.New(cfg, schedulerResources(reopened), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if outcome == "unknown" {
				observedRuns, err := restarted.Runs(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if observedRuns[0].State == workflow.NeedsAttention {
					t.Fatalf("unknown setup did not replay: %+v", observedRuns[0])
				}
			}
			if outcome == "live" {
				observedRuns, err := restarted.Runs(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				observed := observedRuns[0]
				if len(observed.SystemSteps) != 1 || observed.Implementer != nil {
					t.Fatalf("restart duplicated live setup: %+v", observed)
				}
				if err := r.WriteFile(context.Background(), filepath.Join(tree, "release-setup"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			completed := finish(t, restarted, workflow.Active, workflow.Review)
			expected := 1
			if outcome == "unknown" {
				expected = 2
			}
			if len(completed.SystemSteps) != expected || completed.SystemSteps[expected-1].Status != "succeeded" {
				t.Fatalf("restart outcome=%s steps=%+v", outcome, completed.SystemSteps)
			}
		})
	}
}

func setupWorktree(t *testing.T, rt *app.Runtime, id string) string {
	t.Helper()
	var tree string
	// Workspace identity is test fixture wiring, not a behavior assertion.
	if err := rt.DB.QueryRow("SELECT worktree_path FROM runs WHERE id=?", id).Scan(&tree); err != nil {
		t.Fatal(err)
	}
	return tree
}
func awaitSetupSession(t *testing.T, r runner.Runner, ref runner.SessionRef) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, err := r.SessionStatus(context.Background(), ref)
		if err != nil {
			t.Fatal(err)
		}
		if status.State == runner.SessionExited {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("setup did not exit")
}

func TestSetupTimeoutNeedsAttentionWithoutStartingAgent(t *testing.T) {
	_, rt, api, _, cfg, r := localFlow(t, successfulScript)
	cfg.Repositories[0].Setup = []string{"printf 'waiting for dependencies\\n'; /bin/sleep 60"}
	cfg.Repositories[0].CommandTimeout = 150 * time.Millisecond
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(context.Background())
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
	id := runs[0].ID
	t.Cleanup(func() { s.Stop(context.Background(), id) })
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err = s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if runs[0].State == workflow.NeedsAttention {
			run := runs[0]
			if run.LastErrorCode != "workspace.setup_failed" || !strings.Contains(run.LastErrorMessage, "timed out") || !strings.Contains(run.LastErrorMessage, "waiting for dependencies") || run.Implementer != nil {
				t.Fatalf("timeout diagnostic=%+v", run)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("setup timeout was not enforced: %+v", runs[0])
}

func TestSetupRetryRunsConfiguredListBeforeImplementer(t *testing.T) {
	t.Setenv("MERGEYARD_UNRELATED_SECRET", "must not inherit")
	_, rt, api, _, cfg, r := localFlow(t, "[ -f setup-ready ] || exit 9\n"+successfulScript)
	cfg.Repositories[0].Setup = []string{
		`[ "$SETUP_VALUE" = explicit ] && [ -z "${MERGEYARD_UNRELATED_SECRET+x}" ]; if [ ! -f release-setup ]; then printf 'install blocked\n' >&2; exit 7; fi`,
		`setup_state=ready`,
		`[ "$setup_state" = ready ]; printf ready > setup-ready`,
	}
	deps := scheduler.Dependencies{GitHub: api, Runner: r, Env: map[string]string{"SETUP_VALUE": "explicit"}}
	s, err := scheduler.New(cfg, schedulerResources(rt), deps)
	if err != nil {
		t.Fatal(err)
	}
	failed := finish(t, s, workflow.NeedsAttention, workflow.Implement)
	if failed.Implementer != nil || len(failed.SystemSteps) != 1 || !strings.Contains(failed.LastErrorMessage, "install blocked") {
		t.Fatalf("first setup=%+v", failed)
	}
	tree := setupWorktree(t, rt, failed.ID)
	if _, err := r.ReadFile(context.Background(), filepath.Join(tree, "setup-ready")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("setup ran later commands after failure: %v", err)
	}
	if err := r.WriteFile(context.Background(), filepath.Join(tree, "release-setup"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Retry(context.Background(), failed.ID); err != nil {
		t.Fatal(err)
	}
	completed := finish(t, s, workflow.Active, workflow.Review)
	if len(completed.SystemSteps) != 2 || completed.SystemSteps[1].Status != "succeeded" || completed.Implementer == nil || completed.Implementer.Attempt != 1 {
		t.Fatalf("retried setup=%+v", completed)
	}
}

func TestStopDuringSetupPreservesWorkspaceAndEndsOwnedGroup(t *testing.T) {
	_, rt, api, _, cfg, r := localFlow(t, successfulScript)
	cfg.Repositories[0].Setup = []string{`printf preserved > setup-work; printf ready; (/bin/sleep 2; printf survived > child-survived) & /bin/sleep 60`}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(context.Background())
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
	run := runs[0]
	ref, err := s.Watch(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := r.ReadFile(context.Background(), filepath.Join(ref.PhaseDir, "events.jsonl"))
		if strings.Contains(string(data), "ready") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	tree := setupWorktree(t, rt, run.ID)
	if err := s.Stop(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	runs, err = s.Runs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if runs[0].State != workflow.Stopped || runs[0].Implementer != nil || runs[0].SystemSteps[0].Status != "stopped" {
		t.Fatalf("stopped setup=%+v", runs[0])
	}
	if data, err := r.ReadFile(context.Background(), filepath.Join(tree, "setup-work")); err != nil || string(data) != "preserved" {
		t.Fatalf("workspace was not preserved: %q %v", data, err)
	}
	status, err := r.SessionStatus(context.Background(), ref)
	if err != nil || status.State == runner.SessionRunning {
		t.Fatalf("setup group survives Stop: %+v %v", status, err)
	}
	time.Sleep(2100 * time.Millisecond)
	if _, err := r.ReadFile(context.Background(), filepath.Join(tree, "child-survived")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("setup child survives Stop: %v", err)
	}
}

func TestSetupRunsAgainForRecreatedWorktree(t *testing.T) {
	_, rt, api, _, cfg, r := localFlow(t, `printf failed >&2; exit 7`)
	count := filepath.Join(t.TempDir(), "setup-count")
	cfg.Repositories[0].Setup = []string{`printf setup >> "$SETUP_COUNT"`}
	deps := scheduler.Dependencies{GitHub: api, Runner: r, Env: map[string]string{"SETUP_COUNT": count}}
	s, err := scheduler.New(cfg, schedulerResources(rt), deps)
	if err != nil {
		t.Fatal(err)
	}
	failed := finish(t, s, workflow.NeedsAttention, workflow.Implement)
	tree := setupWorktree(t, rt, failed.ID)
	base := filepath.Join(rt.Workspace.Root, "repos", "owner-repo", "base")
	// Recreate the owned worktree on its saved branch, as workspace recovery does.
	gitCommand(t, base, "worktree", "remove", "--force", tree)
	gitCommand(t, base, "worktree", "add", tree, "mergeyard/issue-7")
	if _, err := s.Retry(context.Background(), failed.ID); err != nil {
		t.Fatal(err)
	}
	again := finish(t, s, workflow.NeedsAttention, workflow.Implement)
	data, err := r.ReadFile(context.Background(), count)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "setupsetup" || len(again.SystemSteps) != 2 {
		t.Fatalf("recreated workspace skipped setup: output=%q steps=%+v", data, again.SystemSteps)
	}
}
