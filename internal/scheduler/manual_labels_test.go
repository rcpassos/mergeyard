package scheduler_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// Count publication effects at the real Git adapter boundary: repairing labels
// must not re-inspect, re-commit or re-push an accepted handback.
type handbackLabelGit struct {
	*managedgit.Manager
	inspected, committed, pushed int
}

func (g *handbackLabelGit) InspectHandback(ctx context.Context, run managedgit.Run, target string) (string, error) {
	g.inspected++
	return g.Manager.InspectHandback(ctx, run, target)
}
func (g *handbackLabelGit) CommitHandback(ctx context.Context, run managedgit.Run, previous string) (managedgit.CommitResult, error) {
	g.committed++
	return g.Manager.CommitHandback(ctx, run, previous)
}
func (g *handbackLabelGit) PushHandback(ctx context.Context, run managedgit.Run, previous, target string) error {
	g.pushed++
	return g.Manager.PushHandback(ctx, run, previous, target)
}

func TestBlockedHandbackRepairsLabelsWithoutRepeatingPublicationOrGrant(t *testing.T) {
	for _, kind := range []harness.FailureKind{harness.TemporaryLimit, harness.CreditsExhausted} {
		t.Run(string(kind), func(t *testing.T) {
			_, rt, api, remote, cfg, r := pairingFlow(t, "claude", "claude", disputedReport)
			cfg.MaxRounds, cfg.Concurrency, cfg.Repositories[0].Concurrency = 1, 2, 2
			now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
			h := manualLimitHarness(cfg, "claude", kind)
			g := &handbackLabelGit{Manager: managedgit.New(rt.Workspace)}
			deps := scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r, Git: g, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{"claude": h}}
			s, err := scheduler.New(cfg, schedulerResources(rt), deps)
			if err != nil {
				t.Fatal(err)
			}
			before := finish(t, s, workflow.NeedsAttention, workflow.Review)
			if before.LastErrorCode != "review.max_rounds_exceeded" {
				t.Fatalf("fixture: %+v", before)
			}
			ctx := context.Background()
			command, err := s.Takeover(ctx, before.ID)
			if err != nil {
				t.Fatal(err)
			}
			api.issues["owner/repo"] = append(api.issues["owner/repo"], ready(8))
			replacePhaseScript(t, cfg, workflow.Review, reviewScript(`exit 1`))
			var blocker workflow.Run
			waitForWithin(t, 30*time.Second, func() bool {
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
			if err := os.WriteFile(filepath.Join(command.Dir, "feature.txt"), []byte("manual repair"), 0600); err != nil {
				t.Fatal(err)
			}
			labelFailure := fmt.Errorf("attention label response lost")
			api.mutate = func(action, repo string, n int, label string) error {
				if n == before.IssueNumber && action == "remove" && label == cfg.Repositories[0].Labels.NeedsAttention {
					return labelFailure
				}
				return nil
			}
			selected, err := s.Handback(ctx, before.ID)
			if err == nil || selected.State != workflow.WaitingForHarness || selected.Phase != workflow.Review || selected.ReviewRound != 2 || len(selected.Handbacks) != 1 || selected.Handbacks[0].Pending || selected.Handbacks[0].GrantedRound != 2 {
				t.Fatalf("handback did not finish before label failure: %+v %v", selected, err)
			}
			if !has(api.issues["owner/repo"][0], cfg.Repositories[0].Labels.NeedsAttention) {
				t.Fatal("fixture did not preserve stale attention label")
			}
			published := gitCommand(t, remote, "rev-parse", "mergeyard/issue-7")
			launches := len(h.phases)
			root := rt.Workspace.Root
			if err := rt.Close(); err != nil {
				t.Fatal(err)
			}
			rt, err = app.Open(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			defer rt.Close()
			g.Manager = managedgit.New(rt.Workspace)
			s, err = scheduler.New(cfg, schedulerResources(rt), deps)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Reconcile(ctx); !errors.Is(err, labelFailure) {
				t.Fatalf("waiting reconciliation did not retry the persistent label failure: %v", err)
			}
			api.mutate = nil
			for range 2 {
				if _, err := s.Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
			}
			issue := api.issues["owner/repo"][0]
			if has(issue, cfg.Repositories[0].Labels.NeedsAttention) || has(issue, cfg.Repositories[0].Labels.Ready) || !has(issue, cfg.Repositories[0].Labels.Running) {
				t.Fatalf("labels were not repaired while the harness stayed blocked: %+v", issue.Labels)
			}
			after, err := rt.Workflow.Get(ctx, before.ID)
			if err != nil || after.State != workflow.WaitingForHarness || after.Phase != workflow.Review || after.ReviewRound != 2 || len(after.Handbacks) != 1 || after.Handbacks[0].GrantedRound != 2 || len(h.phases) != launches {
				t.Fatalf("label recovery changed the waiting phase, grant or execution: %+v %v", after, err)
			}
			if g.inspected != 1 || g.committed != 1 || g.pushed != 1 || gitCommand(t, remote, "rev-parse", "mergeyard/issue-7") != published || gitCommand(t, remote, "show", "mergeyard/issue-7:feature.txt") != "manual repair" {
				t.Fatalf("label recovery repeated publication: inspect=%d commit=%d push=%d", g.inspected, g.committed, g.pushed)
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
