package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/review"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestHarnessPairingsReconcileV1ReviewAfterUpgrade(t *testing.T) {
	t.Parallel()

	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			t.Run(implementer+"/"+reviewer, func(t *testing.T) {
				t.Parallel()

				ctx := context.Background()
				gate := filepath.Join(t.TempDir(), "release-v1")
				fix := `printf fixed > feature.txt; ` + testkit.FixedReport
				s, initial, api, _, cfg, r := testkit.TmuxPairingFlow(t, implementer, reviewer, fix)
				legacy := strings.Replace(testkit.ChangesReview, `"schema_version":4,"criteria":[],"human_action":null,"prior_findings":[],`, `"schema_version":1,`, 1)
				held := `while [ ! -f '` + gate + `' ]; do /bin/sleep 0.02; done; `
				testkit.ReplacePhaseScript(t, cfg, workflow.Review, strings.Replace(testkit.LoopScript(fix), testkit.ChangesReview, held+legacy, 1))
				run := testkit.WaitForPhase(t, s, workflow.Review)
				phaseDir := filepath.Join(initial.Workspace.Root, "runs", run.ID, "phases", "review-1-1")
				var schema map[string]any
				if err := json.Unmarshal([]byte(review.Schema), &schema); err != nil {
					t.Fatal(err)
				}
				props := schema["properties"].(map[string]any)
				props["schema_version"].(map[string]any)["const"] = 1
				delete(props, "prior_findings")
				delete(props, "human_action")
				delete(props, "criteria")
				schema["required"] = []string{"schema_version", "status", "summary", "findings"}
				raw, _ := json.Marshal(schema)
				if err := os.WriteFile(filepath.Join(phaseDir, "schema.json"), raw, 0600); err != nil {
					t.Fatal(err)
				}
				root := initial.Workspace.Root
				if err := initial.Close(); err != nil {
					t.Fatal(err)
				}
				runtime, err := app.Open(ctx, root)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { runtime.Close() })
				restarted, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := restarted.Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(gate, nil, 0600); err != nil {
					t.Fatal(err)
				}
				run = testkit.Finish(t, restarted, workflow.Active, workflow.Fix)
				if !run.Review.Accepted || run.Review.Report.SchemaVersion != 1 || run.Review.Report.PriorFindings == nil || len(run.Review.Report.PriorFindings) != 0 {
					t.Fatalf("v1 review failed reconciliation: %+v", run.Review)
				}
				// Recreate the exact v1 stored representation, with no prior_findings key.
				var saved map[string]any
				encoded, _ := json.Marshal(run.Review.Report)
				if err := json.Unmarshal(encoded, &saved); err != nil {
					t.Fatal(err)
				}
				delete(saved, "prior_findings")
				delete(saved, "human_action")
				delete(saved, "criteria")
				encoded, _ = json.Marshal(saved)
				if _, err := runtime.DB.Exec(`UPDATE review_attempts SET report_json=? WHERE attempt_id IN (SELECT id FROM phase_attempts WHERE run_id=? AND round=1 AND phase='review')`, string(encoded), run.ID); err != nil {
					t.Fatal(err)
				}
				if err := runtime.Close(); err != nil {
					t.Fatal(err)
				}
				runtime, err = app.Open(ctx, root)
				if err != nil {
					t.Fatal(err)
				}
				restarted, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := restarted.Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
				run = testkit.Finish(t, restarted, workflow.WaitingForCI, workflow.Review)
				if run.ReviewRound != 2 || run.Review.Report.SchemaVersion != 4 || len(run.Review.Report.PriorFindings) != 2 || len(run.ReviewHistory) != 2 || run.ReviewHistory[0].Report.PriorFindings == nil || api.Creations != 1 {
					t.Fatalf("v1 history failed v2 continuation: %+v", run)
				}
				server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: restarted, Config: cfg})
				if err != nil {
					t.Fatal(err)
				}
				response := httptest.NewRecorder()
				server.ServeHTTP(response, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil))
				body := response.Body.String()
				if response.Code != 200 || !strings.Contains(body, "Criteria were not recorded by this schema version.") {
					t.Fatal("dashboard did not render the v1 review contract")
				}
			})
		}
	}
}

func TestChangedHeadRetryAccountsForAcceptedSameRoundBlockers(t *testing.T) {
	t.Parallel()

	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			for _, complete := range []bool{false, true} {
				name := "omitted"
				report := testkit.ApprovedReview
				if complete {
					name = "accounted"
					report = testkit.ApprovedAfterFixReview
				}
				t.Run(implementer+"/"+reviewer+"/"+name, func(t *testing.T) {
					t.Parallel()

					ctx := context.Background()
					_, rt, api, _, cfg, r := testkit.TmuxPairingFlow(t, implementer, reviewer, testkit.DisputedReport)
					cfg.MaxRounds = 1
					s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
					if err != nil {
						t.Fatal(err)
					}
					before := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Review)
					path := filepath.Join(rt.Workspace.Root, "worktrees", "owner-repo", before.ID)
					if err := os.WriteFile(filepath.Join(path, "feature.txt"), []byte("published repair"), 0600); err != nil {
						t.Fatal(err)
					}
					testkit.GitCommand(t, path, "add", "feature.txt")
					testkit.GitCommand(t, path, "commit", "-m", "published repair")
					testkit.GitCommand(t, path, "push", "origin", "HEAD")
					testkit.ReplacePhaseScript(t, cfg, workflow.Review, testkit.ReviewScript(report))
					selected, err := s.Retry(ctx, before.ID)
					if err != nil {
						t.Fatal(err)
					}
					if selected.ReviewRound != 1 {
						t.Fatalf("retry unexpectedly changed round: %d", selected.ReviewRound)
					}
					// Reload the scheduler to verify the prior identity survives restart.
					s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
					if err != nil {
						t.Fatal(err)
					}
					var after workflow.Run
					testkit.WaitFor(t, func() bool {
						if err := s.Tick(ctx); err != nil {
							t.Fatal(err)
						}
						after, err = rt.Workflow.Get(ctx, before.ID)
						if err != nil {
							t.Fatal(err)
						}
						return after.State == workflow.NeedsAttention || after.State == workflow.WaitingForCI
					})
					input, err := os.ReadFile(filepath.Join(rt.Workspace.Root, "runs", before.ID, "phases", "review-1-2", "input.md"))
					if err != nil || !strings.Contains(string(input), `Prior blocking IDs to account for:
["F1","F2"]`) {
						t.Fatalf("retry prompt lost blocker identities: %v", err)
					}
					if complete {
						if after.State != workflow.WaitingForCI || !after.Review.Accepted || len(after.Review.Report.PriorFindings) != 2 {
							t.Fatalf("accounted blockers not accepted: %s %s", after.State, after.LastErrorMessage)
						}
					} else if after.State != workflow.NeedsAttention || after.LastErrorCode != "phase.result_invalid" || after.Review.Accepted || after.ApprovedSHA != "" {
						t.Fatalf("prior blockers disappeared: %s accepted=%v", after.State, after.Review.Accepted)
					}
					if !complete {
						prior, _, err := review.LoadPriorContext(ctx, rt.DB, before.ID, after.ReviewRound)
						if err != nil || len(prior) != 2 || prior[0].ID != "F1" || prior[1].ID != "F2" {
							t.Fatalf("rejected retry replaced accepted blockers: %+v %v", prior, err)
						}
					}
				})
			}
		}
	}
}
