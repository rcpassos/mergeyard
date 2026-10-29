package integration_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestEveryConfiguredCheckRequiresDurablePassingEvidence(t *testing.T) {
	t.Parallel()

	for _, first := range []string{"printf first; exit 0", "exec /usr/bin/true"} {
		t.Run(first, func(t *testing.T) {
			t.Parallel()

			_, rt, api, remote, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
			cfg.Repositories[0].Checks = []string{first, "printf should-not-run; false"}
			cfg.Repositories[0].CheckRetries = 0
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			var run workflow.Run
			testkit.WaitForWithin(t, 15e9, func() bool {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				runs, err := s.Runs(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				run = runs[0]
				return run.State == workflow.NeedsAttention || run.Phase == workflow.Review
			})
			if run.State != workflow.NeedsAttention || run.LastErrorCode != "checks.failed" || api.Creations != 0 || testkit.GitCommand(t, remote, "branch", "--list", "mergeyard/issue-7") != "" {
				t.Fatalf("incomplete checks published: %+v", run)
			}
			if len(run.SystemSteps) != 1 || run.SystemSteps[0].Status != "failed" || run.SystemSteps[0].Results[1].Status != "skipped" {
				t.Fatalf("incomplete evidence=%+v", run.SystemSteps)
			}
		})
	}
}

func TestRetryNeverReusesChecksThatChangedThePinnedTree(t *testing.T) {
	t.Parallel()

	_, rt, api, remote, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	marker := filepath.Join(t.TempDir(), "mutate")
	if err := r.WriteFile(context.Background(), marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	cfg.Repositories[0].Checks = []string{`if [ -f "$MUTATE" ]; then printf fixed > feature.txt; fi; /usr/bin/grep -q fixed feature.txt`}
	cfg.Repositories[0].CheckRetries = 0
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r, Env: map[string]string{"MUTATE": marker}})
	if err != nil {
		t.Fatal(err)
	}
	failed := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
	tree := testkit.SetupWorktree(t, rt, failed.ID)
	root := rt.Workspace.Root
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := app.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(reopened), scheduler.Dependencies{GitHub: api, Runner: r, Env: map[string]string{"MUTATE": marker}})
	if err != nil {
		t.Fatal(err)
	}
	testkit.GitCommand(t, tree, "restore", "feature.txt")
	if err := r.RemovePath(context.Background(), marker); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Retry(context.Background(), failed.ID); err != nil {
		t.Fatal(err)
	}
	var run workflow.Run
	testkit.WaitForWithin(t, 15e9, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		run = runs[0]
		return run.State == workflow.NeedsAttention || run.Phase == workflow.Review
	})
	if run.State != workflow.NeedsAttention || run.LastErrorCode != "checks.failed" || api.Creations != 0 || len(run.SystemSteps) < 2 || testkit.GitCommand(t, remote, "branch", "--list", "mergeyard/issue-7") != "" {
		t.Fatalf("mutated check evidence authorized untested code: %+v", run)
	}
	if run.SystemSteps[0].Status != "failed" {
		t.Fatalf("changed-tree evidence was not invalidated: %+v", run.SystemSteps)
	}
}

func TestFixAndHandbackInvalidateChecksOfChangedPinnedTrees(t *testing.T) {

	for _, mode := range []string{"fix", "handback"} {
		t.Run(mode, func(t *testing.T) {

			_, rt, api, remote, cfg, r := testkit.TmuxFlow(t, testkit.LoopScript("printf broken > feature.txt; "+testkit.FixedReport))
			marker := filepath.Join(t.TempDir(), "mutate")
			if err := r.WriteFile(context.Background(), marker, nil, 0600); err != nil {
				t.Fatal(err)
			}
			cfg.Repositories[0].Checks = []string{`if /usr/bin/grep -q implemented feature.txt; then :; else if [ -f "$MUTATE" ]; then printf fixed > feature.txt; fi; /usr/bin/grep -q fixed feature.txt; fi`}
			cfg.Repositories[0].CheckRetries = 0
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r, Env: map[string]string{"MUTATE": marker}})
			if err != nil {
				t.Fatal(err)
			}
			var failed workflow.Run
			phase := workflow.Fix
			if mode == "fix" {
				failed = testkit.Finish(t, s, workflow.NeedsAttention, phase)
			} else {
				phase = workflow.Review
				before := testkit.Finish(t, s, workflow.Active, workflow.Review)
				if _, err := s.Takeover(context.Background(), before.ID); err != nil {
					t.Fatal(err)
				}
				tree := testkit.SetupWorktree(t, rt, before.ID)
				if err := r.WriteFile(context.Background(), tree+"/feature.txt", []byte("broken"), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Handback(context.Background(), before.ID); err != nil {
					t.Fatal(err)
				}
				failed = testkit.Finish(t, s, workflow.NeedsAttention, phase)
			}
			invalid := failed.SystemSteps[len(failed.SystemSteps)-1]
			if invalid.Status != "failed" || invalid.PinnedTreeVerified {
				t.Fatalf("changed-tree check evidence survived: %+v", invalid)
			}
			tree := testkit.SetupWorktree(t, rt, failed.ID)
			root := rt.Workspace.Root
			if err := rt.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := app.Open(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { reopened.Close() })
			s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(reopened), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r, Env: map[string]string{"MUTATE": marker}})
			if err != nil {
				t.Fatal(err)
			}
			testkit.GitCommand(t, tree, "restore", "feature.txt")
			if err := r.RemovePath(context.Background(), marker); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Retry(context.Background(), failed.ID); err != nil {
				t.Fatal(err)
			}
			after := testkit.Finish(t, s, workflow.NeedsAttention, phase)
			if after.LastErrorCode != "checks.failed" || len(after.SystemSteps) <= len(failed.SystemSteps) || testkit.GitCommand(t, remote, "show", "mergeyard/issue-7:feature.txt") != "implemented" {
				t.Fatalf("restored tree reused invalid checks: %+v", after)
			}
			if mode == "handback" && (after.Implementer.Attempt != 1 || len(after.FixHistory) != 0) {
				t.Fatalf("handback auto-fixed manual work: %+v", after)
			}
		})
	}
}
