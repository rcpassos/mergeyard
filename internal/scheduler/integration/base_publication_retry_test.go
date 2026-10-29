package integration_test

import (
	"context"
	"testing"

	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestRetryCommittedBaseRepairResumesPublication(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"before push", "lost acknowledgement", "local head replaced"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			_, rt, base, remote, cfg, r := testkit.TmuxFlow(t, testkit.CiFixScript(`printf '%s\n' implemented > feature.txt; `+testkit.CiFixedReport))
			cfg.Repositories[0].Checks = []string{"test -f feature.txt"}
			api := testkit.PassingBaseAPI(base)
			g := &testkit.InterruptedBasePush{Manager: testkit.TestGitManager(rt.Workspace), Fail: true, Applied: mode == "lost acknowledgement"}
			deps := scheduler.Dependencies{GitHub: testkit.CiBranchGitHub{CiGitHub: api}, Git: g, Runner: r}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
			if err != nil {
				t.Fatal(err)
			}
			approved := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
			baseSHA := testkit.AdvanceBase(t, remote, "feature.txt", "conflicting base\n")
			failed := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Fix)
			if failed.LastErrorCode != "git.push_failed" || failed.Fix.CommitSHA == "" || failed.Fix.Pushed || failed.Fix.Status != "succeeded" {
				t.Fatalf("fixture has no saved unpublished fix: %+v", failed.Fix)
			}
			tree := testkit.SetupWorktree(t, rt, failed.ID)
			if mode == "local head replaced" {
				testkit.GitCommand(t, tree, "commit", "--allow-empty", "-m", "unrecorded local commit")
				if _, err := s.Retry(context.Background(), failed.ID); err == nil {
					t.Fatal("retry accepted unrecorded local head")
				}
				if got := testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7"); got != approved.ApprovedSHA {
					t.Fatal("rejected retry published unrelated work")
				}
				return
			}
			// Restart before explicit retry to exercise persisted commit ownership.
			s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Retry(context.Background(), failed.ID); err != nil {
				t.Fatalf("saved merge commit rejected: %v", err)
			}
			ready := testkit.Finish(t, s, workflow.ReadyToMerge, workflow.Review)
			if ready.ReviewRound != 2 || len(ready.FixHistory) != 1 || ready.Fix.Attempt != 1 || ready.Fix.CommitSHA != failed.Fix.CommitSHA || !ready.Fix.Pushed {
				t.Fatalf("retry recreated instead of publishing saved fix: %+v", ready.Fix)
			}
			if got := testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7"); got != failed.Fix.CommitSHA {
				t.Fatal("published a different commit")
			}
			testkit.GitCommand(t, remote, "merge-base", "--is-ancestor", baseSHA, ready.ApprovedSHA)
		})
	}
}
