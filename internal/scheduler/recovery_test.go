package scheduler_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestMissingRoleSessionRecoveryAcrossPhases(t *testing.T) {
	t.Parallel()

	for _, agent := range []string{"claude", "codex"} {
		for _, phase := range []workflow.Phase{workflow.Implement, workflow.Review, workflow.Fix} {
			t.Run(agent+"/"+string(phase), func(t *testing.T) {
				flow := testkit.NewRecoveryScenario(t, agent, phase, "success")
				s, runtime, api, _, cfg, _ := flow.Parts()
				state, finalPhase := workflow.WaitingForCI, workflow.Review
				if phase == workflow.Implement {
					state = workflow.Active
				}
				run := flow.Until(s, state, finalPhase)
				if len(run.SessionRecoveries) != 1 || run.SessionRecoveries[0].SessionID == "" || run.SessionRecoveries[0].SessionID == run.SessionRecoveries[0].PreviousSessionID {
					t.Fatalf("lost warning or identity: %+v", run.SessionRecoveries)
				}
				round := 1
				if phase == workflow.Implement {
					round = 0
				}
				if phase == workflow.Review {
					round = 2
				}
				input, err := os.ReadFile(fmt.Sprintf("%s/runs/%s/phases/%s-%d-2/input.md", runtime.Workspace.Root, run.ID, phase, round))
				for _, value := range []string{"Implement this", "owner/repo", "#7"} {
					if err != nil || !strings.Contains(string(input), value) {
						t.Fatalf("missing %q: %s %v", value, input, err)
					}
				}
				if phase != workflow.Implement {
					for _, value := range []string{"F1", "F2", "feature.txt"} {
						if !strings.Contains(string(input), value) {
							t.Fatalf("lost findings/diff %q: %s", value, input)
						}
					}
				}
				if phase == workflow.Review && (!strings.Contains(string(input), "Changed source") || run.ReviewRound != 2 || run.Review.Attempt != 2 || !run.Review.Accepted) {
					t.Fatalf("lost prior fix or changed round: %+v", run)
				}
				if api.Creations != 1 || fmt.Sprint(flow.Git.Sequence-1) != map[workflow.Phase]string{workflow.Implement: "1", workflow.Review: "2", workflow.Fix: "2"}[phase] {
					t.Fatal("duplicate publication")
				}
				server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
				if err != nil {
					t.Fatal(err)
				}
				page := httptest.NewRecorder()
				server.ServeHTTP(page, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil))
				for _, value := range []string{"harness.session_resume_failed", "continuing once in a fresh session", run.SessionRecoveries[0].SessionID, run.SessionRecoveries[0].PreviousSessionID} {
					if page.Code != 200 || !strings.Contains(page.Body.String(), value) {
						t.Fatalf("detail/timeline missing %q: %s", value, page.Body.String())
					}
				}
			})
		}
	}
}

func TestMissingSessionFreshResultAndUnrelatedStartupRemainBounded(t *testing.T) {
	t.Parallel()

	for _, agent := range []string{"claude", "codex"} {
		for _, phase := range []workflow.Phase{workflow.Implement, workflow.Review, workflow.Fix} {
			for _, outcome := range []string{"invalid", "unrelated", "failed"} {
				t.Run(agent+"/"+string(phase)+"/"+outcome, func(t *testing.T) {
					flow := testkit.NewRecoveryScenario(t, agent, phase, outcome)
					s, runtime, api, _, cfg, r := flow.Parts()
					run := flow.Until(s, workflow.NeedsAttention, phase)
					expected := 0
					if outcome == "invalid" || outcome == "failed" {
						expected = 1
						if outcome == "invalid" && run.LastErrorCode != "phase.result_invalid" {
							t.Fatalf("accepted invalid result: %+v", run)
						}
					}
					if outcome == "failed" && run.LastErrorCode != "phase.retries_exhausted" && run.LastErrorCode != "phase.execution_failed" {
						t.Fatalf("normal attempt budget changed: %+v", run)
					}
					if len(run.SessionRecoveries) != expected || run.ApprovedSHA != "" {
						t.Fatalf("unbounded recovery: %+v", run)
					}
					restarted, err := flow.New(cfg, scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
					if err != nil {
						t.Fatal(err)
					}
					for range 2 {
						if _, err := restarted.Reconcile(context.Background()); err != nil {
							t.Fatal(err)
						}
					}
					saved, err := runtime.Workflow.Get(context.Background(), run.ID)
					if err != nil || len(saved.SessionRecoveries) != expected || saved.State != workflow.NeedsAttention {
						t.Fatalf("restart replenished recovery: %+v %v", saved, err)
					}
				})
			}
		}
	}
}

func TestMissingSessionRecoveryAllowanceCannotBeReused(t *testing.T) {
	t.Parallel()

	for _, agent := range []string{"claude", "codex"} {
		for _, phase := range []workflow.Phase{workflow.Implement, workflow.Review, workflow.Fix} {
			t.Run(agent+"/"+string(phase), func(t *testing.T) {
				flow := testkit.NewRecoveryScenario(t, agent, phase, "exhausted")
				s, runtime, api, _, cfg, r := flow.Parts()
				run := flow.Until(s, workflow.NeedsAttention, phase)
				if len(run.SessionRecoveries) != 1 || run.LastErrorCode != "harness.session_resume_failed" {
					t.Fatalf("allowance replenished: %+v", run)
				}
				restarted, err := flow.New(cfg, scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := restarted.Reconcile(context.Background()); err != nil {
					t.Fatal(err)
				}
				var attempts int
				if err := runtime.DB.QueryRow("SELECT count(*) FROM phase_attempts WHERE run_id=? AND phase=? AND round=?", run.ID, phase, map[workflow.Phase]int{workflow.Implement: 0, workflow.Fix: 1, workflow.Review: 2}[phase]).Scan(&attempts); err != nil {
					t.Fatal(err)
				}
				if attempts != 3 {
					t.Fatalf("second recovery launched: %d", attempts)
				}
			})
		}
	}
}
