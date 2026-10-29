package integration_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestBlockedHandbackRepairsLabelsWithoutRepeatingPublicationOrGrant(t *testing.T) {

	for _, kind := range []harness.FailureKind{harness.TemporaryLimit, harness.CreditsExhausted} {
		t.Run(string(kind), func(t *testing.T) {

			_, rt, api, remote, cfg, r := testkit.TmuxPairingFlow(t, "claude", "claude", testkit.DisputedReport)
			cfg.MaxRounds, cfg.Concurrency, cfg.Repositories[0].Concurrency = 1, 2, 2
			now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
			h := testkit.ManualLimitHarness(cfg, "claude", kind)
			g := &testkit.HandbackLabelGit{Manager: testkit.TestGitManager(rt.Workspace)}
			deps := scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r, Git: g, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{"claude": h}}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
			if err != nil {
				t.Fatal(err)
			}
			before := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Review)
			if before.LastErrorCode != "review.max_rounds_exceeded" {
				t.Fatalf("fixture: %+v", before)
			}
			ctx := context.Background()
			prepared, err := s.Takeover(ctx, before.ID)
			if err != nil {
				t.Fatal(err)
			}
			api.Issues["owner/repo"] = append(api.Issues["owner/repo"], testkit.Ready(8))
			testkit.ReplacePhaseScript(t, cfg, workflow.Review, testkit.ReviewScript(`exit 1`))
			var blocker workflow.Run
			testkit.WaitForWithin(t, 30*time.Second, func() bool {
				if err := s.Tick(ctx); err != nil {
					t.Fatal(err)
				}
				runs, err := s.Runs(ctx)
				if err != nil {
					t.Fatal(err)
				}
				for _, run := range runs {
					if run.IssueNumber == 8 {
						blocker = run
					}
				}
				return blocker.HarnessWait != nil
			})
			if err := s.Stop(ctx, blocker.ID); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(testkit.RunWorktree(t, rt.Workspace.Root, prepared), "feature.txt"), []byte("manual repair"), 0600); err != nil {
				t.Fatal(err)
			}
			labelFailure := fmt.Errorf("attention label response lost")
			api.Mutate = func(action, repo string, n int, label string) error {
				if n == before.IssueNumber && action == "remove" && label == cfg.Repositories[0].Labels.NeedsAttention {
					return labelFailure
				}
				return nil
			}
			selected, err := s.Handback(ctx, before.ID)
			if err == nil || selected.State != workflow.WaitingForHarness || selected.Phase != workflow.Review || selected.ReviewRound != 2 || len(selected.Handbacks) != 1 || selected.Handbacks[0].Pending || selected.Handbacks[0].GrantedRound != 2 {
				t.Fatalf("handback did not finish before label failure: %+v %v", selected, err)
			}
			if !testkit.Has(api.Issues["owner/repo"][0], cfg.Repositories[0].Labels.NeedsAttention) {
				t.Fatal("fixture did not preserve stale attention label")
			}
			published := testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7")
			launches := len(h.Phases)
			root := rt.Workspace.Root
			if err := rt.Close(); err != nil {
				t.Fatal(err)
			}
			rt, err = app.Open(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			defer rt.Close()
			g.Manager = testkit.TestGitManager(rt.Workspace)
			s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Reconcile(ctx); !errors.Is(err, labelFailure) {
				t.Fatalf("waiting reconciliation did not retry the persistent label failure: %v", err)
			}
			api.Mutate = nil
			for range 2 {
				if _, err := s.Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
			}
			issue := api.Issues["owner/repo"][0]
			if testkit.Has(issue, cfg.Repositories[0].Labels.NeedsAttention) || testkit.Has(issue, cfg.Repositories[0].Labels.Ready) || !testkit.Has(issue, cfg.Repositories[0].Labels.Running) {
				t.Fatalf("labels were not repaired while the harness stayed blocked: %+v", issue.Labels)
			}
			after, err := rt.Workflow.Get(ctx, before.ID)
			if err != nil || after.State != workflow.WaitingForHarness || after.Phase != workflow.Review || after.ReviewRound != 2 || len(after.Handbacks) != 1 || after.Handbacks[0].GrantedRound != 2 || len(h.Phases) != launches {
				t.Fatalf("label recovery changed the waiting phase, grant or execution: %+v %v", after, err)
			}
			if g.Inspected != 1 || g.Committed != 1 || g.Pushed != 1 || testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7") != published || testkit.GitCommand(t, remote, "show", "mergeyard/issue-7:feature.txt") != "manual repair" {
				t.Fatalf("label recovery repeated publication: inspect=%d commit=%d push=%d", g.Inspected, g.Committed, g.Pushed)
			}
			states, err := s.HarnessAvailability(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, state := range states {
				if state.Harness == "claude" && (state.Available || state.ProbeID != "") {
					t.Fatalf("label repair bypassed harness recovery: %+v", state)
				}
			}
			history, err := rt.Events.History(ctx, 0, 200)
			if err != nil {
				t.Fatal(err)
			}
			handbacks := 0
			for _, event := range history {
				if event.RunID == before.ID && event.Type == "run.handed_back" {
					handbacks++
				}
			}
			if handbacks != 1 {
				t.Fatalf("label recovery repeated grant selection: %d", handbacks)
			}
		})
	}
}
