package scheduler_test

import (
	"context"
	"testing"

	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type interruptedBasePush struct {
	*managedgit.Manager
	fail    bool
	applied bool
}

func (g *interruptedBasePush) PushFix(ctx context.Context, run managedgit.Run, previous, target string) error {
	if g.fail {
		g.fail = false
		if g.applied {
			if err := g.Manager.PushFix(ctx, run, previous, target); err != nil {
				return err
			}
		}
		return &fault.Error{Code: "git.push_failed", Message: "simulated interrupted publication"}
	}
	return g.Manager.PushFix(ctx, run, previous, target)
}

func TestRetryCommittedBaseRepairResumesPublication(t *testing.T) {
	for _, mode := range []string{"before push", "lost acknowledgement", "local head replaced"} {
		t.Run(mode, func(t *testing.T) {
			_, rt, base, remote, cfg, r := localFlow(t, ciFixScript(`printf '%s\n' implemented > feature.txt; `+ciFixedReport))
			cfg.Repositories[0].Checks = []string{"test -f feature.txt"}
			api := passingBaseAPI(base)
			g := &interruptedBasePush{Manager: managedgit.New(rt.Workspace), fail: true, applied: mode == "lost acknowledgement"}
			deps := scheduler.Dependencies{GitHub: ciBranchGitHub{api}, Git: g, Runner: r}
			s, err := scheduler.New(cfg, schedulerResources(rt), deps)
			if err != nil {
				t.Fatal(err)
			}
			approved := finish(t, s, workflow.WaitingForCI, workflow.Review)
			baseSHA := advanceBase(t, remote, "feature.txt", "conflicting base\n")
			failed := finish(t, s, workflow.NeedsAttention, workflow.Fix)
			if failed.LastErrorCode != "git.push_failed" || failed.Fix.CommitSHA == "" || failed.Fix.Pushed || failed.Fix.Status != "succeeded" {
				t.Fatalf("fixture has no saved unpublished fix: %+v", failed.Fix)
			}
			tree := setupWorktree(t, rt, failed.ID)
			if mode == "local head replaced" {
				gitCommand(t, tree, "commit", "--allow-empty", "-m", "unrecorded local commit")
				if _, err := s.Retry(context.Background(), failed.ID); err == nil {
					t.Fatal("retry accepted unrecorded local head")
				}
				if got := gitCommand(t, remote, "rev-parse", "mergeyard/issue-7"); got != approved.ApprovedSHA {
					t.Fatal("rejected retry published unrelated work")
				}
				return
			}
			// Restart before explicit retry to exercise persisted commit ownership.
			s, err = scheduler.New(cfg, schedulerResources(rt), deps)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Retry(context.Background(), failed.ID); err != nil {
				t.Fatalf("saved merge commit rejected: %v", err)
			}
			ready := finish(t, s, workflow.ReadyToMerge, workflow.Review)
			if ready.ReviewRound != 2 || len(ready.FixHistory) != 1 || ready.Fix.Attempt != 1 || ready.Fix.CommitSHA != failed.Fix.CommitSHA || !ready.Fix.Pushed {
				t.Fatalf("retry recreated instead of publishing saved fix: %+v", ready.Fix)
			}
			if got := gitCommand(t, remote, "rev-parse", "mergeyard/issue-7"); got != failed.Fix.CommitSHA {
				t.Fatal("published a different commit")
			}
			gitCommand(t, remote, "merge-base", "--is-ancestor", baseSHA, ready.ApprovedSHA)
		})
	}
}
