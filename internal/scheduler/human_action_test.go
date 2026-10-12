package scheduler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

const blockedAction = `"human_action":{"decision":"Reject duplicates or overwrite <script>them</script>?","files":["feature.txt"],"options":["Reject","Overwrite"]}`

func TestHumanActionAcrossAllHarnessPairingsAndPhases(t *testing.T) {
	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			for _, phase := range []workflow.Phase{workflow.Implement, workflow.Review, workflow.Fix} {
				t.Run(implementer+"/"+reviewer+"/"+string(phase), func(t *testing.T) {
					blockedFix := `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":3,"criteria":[],"status":"blocked","summary":"Need a decision",` + blockedAction + `,"responses":[]}}'`
					s, runtime, _, _, cfg, _ := pairingFlow(t, implementer, reviewer, blockedFix)
					script := loopScript(blockedFix)
					if phase == workflow.Implement {
						script = strings.Replace(script, successfulScript, `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":3,"criteria":[],"status":"blocked","summary":"Need a decision",`+blockedAction+`}}'`, 1)
					}
					if phase == workflow.Review {
						script = strings.Replace(script, changesReview, `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":4,"criteria":[],"status":"blocked","summary":"",`+blockedAction+`,"findings":[],"prior_findings":[]}}'`, 1)
					}
					replacePhaseScript(t, cfg, phase, script)
					run := finish(t, s, workflow.NeedsAttention, phase)
					expected := &fault.HumanAction{Decision: "Reject duplicates or overwrite <script>them</script>?", Files: []string{"feature.txt"}, Options: []string{"Reject", "Overwrite"}}
					if run.LastErrorCode != "phase.blocked" || !reflect.DeepEqual(run.HumanAction, expected) {
						t.Fatalf("agent action lost: %+v", run)
					}
					server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
					if err != nil {
						t.Fatal(err)
					}
					for _, path := range []string{"/", "/runs/" + run.ID} {
						response := httptest.NewRecorder()
						server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331"+path, nil))
						for _, text := range []string{"Human action required", "Reject duplicates or overwrite &lt;script&gt;them&lt;/script&gt;?", "feature.txt", "Overwrite"} {
							if !strings.Contains(response.Body.String(), text) {
								t.Fatalf("missing %q in %s: %s", text, path, response.Body.String())
							}
						}
						if strings.Contains(response.Body.String(), "<script>them</script>") {
							t.Fatal("unescaped agent text")
						}
					}
				})
			}
		}
	}
}

func TestAttentionCommentRetriesAfterRestartWithoutDuplicating(t *testing.T) {
	for _, withPR := range []bool{false, true} {
		t.Run(map[bool]string{false: "issue", true: "PR"}[withPR], func(t *testing.T) {
			ctx := context.Background()
			_, initial, api, _, cfg, r := localFlow(t, loopScript(disputedReport))
			cfg.PRComments = true
			publisher := &reportGitHub{branchGitHub: branchGitHub{api}}
			s, err := scheduler.New(cfg, schedulerResources(initial), scheduler.Dependencies{GitHub: publisher, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			var run workflow.Run
			if withPR {
				run = finish(t, s, workflow.WaitingForCI, workflow.Review)
			} else {
				run, err = initial.Workflow.Transition(ctx, "preparation-block", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7})
				if err != nil {
					t.Fatal(err)
				}
			}
			action := &fault.HumanAction{Decision: "Choose `reject` or overwrite <!-- marker -->", Files: []string{"feature.txt"}, Options: []string{"Reject", "Overwrite"}}
			if _, err := initial.Workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.OperationFailed, Failure: &fault.Error{Code: "phase.blocked", Message: "Need decision", HumanAction: action}}); err != nil {
				t.Fatal(err)
			}
			publisher.ambiguous = true
			if _, err := s.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			var frozen string
			var target int
			if err := initial.DB.QueryRow(`SELECT body,issue_number FROM attention_publications WHERE run_id=?`, run.ID).Scan(&frozen, &target); err != nil {
				t.Fatal(err)
			}
			want := 7
			if withPR {
				want = run.PRNumber
			}
			if target != want {
				t.Fatalf("comment target %d want %d", target, want)
			}
			for _, text := range []string{"Human action required", `"human_action"`, `"files"`, `"options"`, "Choose \\u0060reject\\u0060 or overwrite"} {
				if !strings.Contains(frozen, text) {
					t.Fatalf("comment missing %q: %s", text, frozen)
				}
			}
			count := len(publisher.bodies)
			root := initial.Workspace.Root
			if err := initial.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, err := app.Open(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			publisher.failure = false
			s, err = scheduler.New(cfg, schedulerResources(restarted), scheduler.Dependencies{GitHub: publisher, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			// The pending comment survives clearing the current fault.
			if _, err := restarted.Workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.Stop}); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if _, err := s.Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
			}
			var state, body string
			if err := restarted.DB.QueryRow(`SELECT state,body FROM attention_publications WHERE run_id=?`, run.ID).Scan(&state, &body); err != nil {
				t.Fatal(err)
			}
			if state != "published" || body != frozen || len(publisher.bodies) != count {
				t.Fatalf("comment replay changed: %s %d/%d", state, len(publisher.bodies), count)
			}
		})
	}
}

func TestHumanActionRoundExhaustionNamesCurrentBlockers(t *testing.T) {
	_, runtime, api, _, cfg, r := localFlow(t, loopScript(disputedReport))
	cfg.MaxRounds = 1
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.NeedsAttention, workflow.Review)
	if run.LastErrorCode != "review.max_rounds_exceeded" || run.HumanAction == nil || !strings.Contains(run.HumanAction.Decision, "F1, F2") || !reflect.DeepEqual(run.HumanAction.Files, []string{"feature.txt"}) || len(run.HumanAction.Options) != 2 {
		t.Fatalf("missing blockers: %+v", run)
	}
}

func TestReconcileLegacyImplementAndFixSchemas(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		for _, phase := range []workflow.Phase{workflow.Implement, workflow.Fix} {
			t.Run(agent+"/"+string(phase), func(t *testing.T) {
				gate := filepath.Join(t.TempDir(), "release")
				fix := `printf fixed > feature.txt; ` + fixedReport
				s, initial, api, _, cfg, r := pairingFlow(t, agent, "claude", fix)
				legacy := strings.ReplaceAll(loopScript(fix), `"schema_version":3,"criteria":[],"human_action":null,`, `"schema_version":1,`)
				held := `while [ ! -f '` + gate + `' ]; do /bin/sleep 0.02; done; `
				// Gate only the phase whose schema is being restored.
				if phase == workflow.Implement {
					legacy = strings.Replace(legacy, strings.ReplaceAll(successfulScript, `"schema_version":3,"criteria":[],"human_action":null,`, `"schema_version":1,`), held+strings.ReplaceAll(successfulScript, `"schema_version":3,"criteria":[],"human_action":null,`, `"schema_version":1,`), 1)
				} else {
					legacy = strings.Replace(legacy, strings.ReplaceAll(fix, `"schema_version":3,"criteria":[],"human_action":null,`, `"schema_version":1,`), held+strings.ReplaceAll(fix, `"schema_version":3,"criteria":[],"human_action":null,`, `"schema_version":1,`), 1)
				}
				// Other phases must emit their current contract.
				if phase == workflow.Fix {
					legacy = strings.Replace(legacy, strings.ReplaceAll(successfulScript, `"schema_version":3,"criteria":[],"human_action":null,`, `"schema_version":1,`), successfulScript, 1)
				}
				replacePhaseScript(t, cfg, phase, legacy)
				run := waitForPhase(t, s, phase)
				dir := filepath.Join(initial.Workspace.Root, "runs", run.ID, "phases", string(phase)+"-0-1")
				if phase == workflow.Fix {
					dir = filepath.Join(initial.Workspace.Root, "runs", run.ID, "phases", "fix-1-1")
				}
				schema, err := os.ReadFile(filepath.Join(dir, "schema.json"))
				if err != nil {
					t.Fatal(err)
				}
				schema = []byte(strings.Replace(string(schema), `"const":3`, `"const":1`, 1))
				// Only the const is read on reconciliation; original outputs retain their strict v1 shape.
				if err := os.WriteFile(filepath.Join(dir, "schema.json"), schema, 0600); err != nil {
					t.Fatal(err)
				}
				root := initial.Workspace.Root
				if err := initial.Close(); err != nil {
					t.Fatal(err)
				}
				runtime, err := app.Open(context.Background(), root)
				if err != nil {
					t.Fatal(err)
				}
				defer runtime.Close()
				restarted, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(gate, nil, 0600); err != nil {
					t.Fatal(err)
				}
				if phase == workflow.Implement {
					finish(t, restarted, workflow.Active, workflow.Review)
				} else {
					finish(t, restarted, workflow.WaitingForCI, workflow.Review)
				}
			})
		}
	}
}

func TestBlockedActionSurvivesFailedAttemptRestart(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		for _, phase := range []workflow.Phase{workflow.Implement, workflow.Fix} {
			t.Run(agent+"/"+string(phase), func(t *testing.T) {
				blocked := `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":3,"criteria":[],"status":"blocked","summary":"Need decision",` + blockedAction + `,"responses":[]}}'`
				s, initial, api, _, cfg, r := pairingFlow(t, agent, "claude", blocked)
				if phase == workflow.Implement {
					replacePhaseScript(t, cfg, phase, strings.Replace(loopScript(blocked), successfulScript, `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":3,"criteria":[],"status":"blocked","summary":"Need decision",`+blockedAction+`}}'`, 1))
				}
				run := finish(t, s, workflow.NeedsAttention, phase)
				expected := run.HumanAction
				// Recreate the durable boundary after the failed-attempt commit but before
				// its attention transition. The saved result and attempt fault remain.
				if _, err := initial.DB.Exec(`UPDATE runs SET state='ACTIVE',last_error_code=NULL,last_error_message=NULL,human_action_json=NULL WHERE id=?`, run.ID); err != nil {
					t.Fatal(err)
				}
				root := initial.Workspace.Root
				if err := initial.Close(); err != nil {
					t.Fatal(err)
				}
				runtime, err := app.Open(context.Background(), root)
				if err != nil {
					t.Fatal(err)
				}
				defer runtime.Close()
				restarted, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
				if err != nil {
					t.Fatal(err)
				}
				resumed := finish(t, restarted, workflow.NeedsAttention, phase)
				if !reflect.DeepEqual(resumed.HumanAction, expected) {
					t.Fatalf("blocked action lost across failed-attempt restart: got %+v want %+v", resumed.HumanAction, expected)
				}
			})
		}
	}
}
