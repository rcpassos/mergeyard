package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestCIReadinessAfterMergedImplementerFlows(t *testing.T) {

	for _, mode := range []string{"Claude fix and re-review", "Codex implementation"} {
		t.Run(mode, func(t *testing.T) {
			setup := testkit.TmuxFlow
			script := testkit.LoopScript(`printf fixed > feature.txt; ` + testkit.FixedReport)
			if mode == "Codex implementation" {
				setup = testkit.TmuxCodexFlow
				script = testkit.CodexImplementation
			}
			_, runtime, base, _, cfg, r := setup(t, script)
			now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
			api := &testkit.CiGitHub{FakeGitHub: base, ApplyReady: true, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "success"}}}}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.CiBranchGitHub{CiGitHub: api}, Runner: r, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
			if run.CI == nil || run.CI.SHA != run.ApprovedSHA || !run.CI.StartedAt.Equal(now) {
				t.Fatal("approval did not start a pinned CI wait")
			}
			if mode == "Claude fix and re-review" {
				if run.ReviewRound != 2 || run.Fix == nil || !run.Fix.Pushed {
					t.Fatal("fix history lost")
				}
			} else if run.Implementer == nil || run.Implementer.Agent != "codex" || run.Implementer.SessionID != testkit.CodexID {
				t.Fatal("Codex session lost")
			}
			if _, err := s.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			saved, err := runtime.Workflow.Get(context.Background(), run.ID)
			if err != nil || saved.State != workflow.ReadyToMerge || api.ReadyCalls != 1 || saved.CI.CurrentHead != saved.ApprovedSHA {
				t.Fatalf("merged flow readiness: %+v %v", saved, err)
			}
		})
	}
}
