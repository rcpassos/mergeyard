package scheduler_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/review"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestHarnessPairingsRejectInvalidPriorOutcomes(t *testing.T) {
	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			for _, tc := range []struct{ name, report string }{
				{"missing", strings.Replace(approvedAfterFixReview, `{"finding_id":"F1","outcome":"fixed","note":"Source repaired"},`, "", 1)},
				{"unknown", strings.Replace(approvedAfterFixReview, `"finding_id":"F1"`, `"finding_id":"unknown"`, 1)},
				{"not fixed omitted", strings.Replace(approvedAfterFixReview, `"outcome":"fixed"`, `"outcome":"not_fixed"`, 1)},
				{"dispute called fixed", strings.Replace(approvedAfterFixReview, `"outcome":"no_longer_applies"`, `"outcome":"fixed"`, 1)},
			} {
				t.Run(implementer+"/"+reviewer+"/"+tc.name, func(t *testing.T) {
					fix := `printf fixed > feature.txt; ` + fixedReport
					_, runtime, api, _, cfg, r := pairingFlow(t, implementer, reviewer, fix)
					if tc.name == "missing" {
						cfg.Repositories[0].Reviewer.MaxAttempts = 2
					}
					replacePhaseScript(t, cfg, workflow.Review, strings.Replace(loopScript(fix), approvedAfterFixReview, tc.report, 1))
					s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
					if err != nil {
						t.Fatal(err)
					}
					run := finish(t, s, workflow.NeedsAttention, workflow.Review)
					if run.LastErrorCode != "phase.result_invalid" || run.ReviewRound != 2 || run.Review.Accepted || run.ApprovedSHA != "" || run.Review.Attempt != cfg.Repositories[0].Reviewer.MaxAttempts {
						t.Fatalf("invalid prior outcomes bypassed result failure path: %+v", run)
					}
				})
			}
		}
	}
}

func TestHarnessPairingsReconcileV1ReviewAfterUpgrade(t *testing.T) {
	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			t.Run(implementer+"/"+reviewer, func(t *testing.T) {
				ctx := context.Background()
				gate := filepath.Join(t.TempDir(), "release-v1")
				fix := `printf fixed > feature.txt; ` + fixedReport
				s, initial, api, _, cfg, r := pairingFlow(t, implementer, reviewer, fix)
				legacy := strings.Replace(changesReview, `"schema_version":2,"prior_findings":[],`, `"schema_version":1,`, 1)
				held := `while [ ! -f '` + gate + `' ]; do /bin/sleep 0.02; done; `
				replacePhaseScript(t, cfg, workflow.Review, strings.Replace(loopScript(fix), changesReview, held+legacy, 1))
				run := waitForPhase(t, s, workflow.Review)
				phaseDir := filepath.Join(initial.Workspace.Root, "runs", run.ID, "phases", "review-1-1")
				var schema map[string]any
				if err := json.Unmarshal([]byte(review.Schema), &schema); err != nil {
					t.Fatal(err)
				}
				props := schema["properties"].(map[string]any)
				props["schema_version"].(map[string]any)["const"] = 1
				delete(props, "prior_findings")
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
				restarted, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := restarted.Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(gate, nil, 0600); err != nil {
					t.Fatal(err)
				}
				run = finish(t, restarted, workflow.Active, workflow.Fix)
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
				restarted, err = scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := restarted.Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
				run = finish(t, restarted, workflow.WaitingForCI, workflow.Review)
				if run.ReviewRound != 2 || run.Review.Report.SchemaVersion != 2 || len(run.Review.Report.PriorFindings) != 2 || len(run.ReviewHistory) != 2 || run.ReviewHistory[0].Report.PriorFindings == nil || api.creations != 1 {
					t.Fatalf("v1 history failed v2 continuation: %+v", run)
				}
			})
		}
	}
}
