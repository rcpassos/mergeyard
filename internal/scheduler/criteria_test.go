package scheduler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestCriteriaCarryThroughAllHarnessPairings(t *testing.T) {
	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			t.Run(implementer+"/"+reviewer, func(t *testing.T) {
				fix := `printf fixed > feature.txt; ` + fixedReport
				_, runtime, api, _, cfg, r := pairingFlow(t, implementer, reviewer, fix)
				api.issues["owner/repo"][0].Body += "\n## Acceptance criteria\n- [ ] Feature works\n"
				script := loopScript(fix)
				evidence := `"criteria":[{"id":"AC1","met":true,"evidence":"go test: passed <script>evidence</script>"}]`
				verdict := `"criteria":[{"id":"AC1","verdict":"met","note":"Spot-check: feature test passed <script>note</script>"}]`
				script = strings.ReplaceAll(script, `"schema_version":3,"criteria":[]`, `"schema_version":3,`+evidence)
				script = strings.ReplaceAll(script, `"schema_version":4,"criteria":[]`, `"schema_version":4,`+verdict)
				replacePhaseScript(t, cfg, workflow.Implement, script)
				replacePhaseScript(t, cfg, workflow.Review, script)
				publisher := &reportGitHub{branchGitHub: branchGitHub{api}}
				s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: publisher, Runner: r})
				if err != nil {
					t.Fatal(err)
				}
				run := finish(t, s, workflow.WaitingForCI, workflow.Review)
				if len(run.Review.Report.Criteria) != 1 || len(run.Fix.Report.Criteria) != 1 {
					t.Fatal("criteria lost from reports")
				}
				input, err := os.ReadFile(filepath.Join(runtime.Workspace.Root, "runs", run.ID, "phases", "review-1-1", "input.md"))
				if err != nil || !strings.Contains(string(input), "go test: passed") {
					t.Fatalf("implement evidence missing: %s %v", input, err)
				}
				for _, phase := range []string{"implement-0-1", "fix-1-1", "review-1-1", "review-2-1"} {
					input, err := os.ReadFile(filepath.Join(runtime.Workspace.Root, "runs", run.ID, "phases", phase, "input.md"))
					if err != nil || !strings.Contains(string(input), "AC1: Feature works") {
						t.Fatalf("criteria missing in %s: %v", phase, err)
					}
				}
				comments := strings.Join(publisher.bodies, "\n")
				if !strings.Contains(comments, `"criteria"`) || !strings.Contains(comments, `"AC1"`) {
					t.Fatal("comments missing criteria")
				}
				server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
				if err != nil {
					t.Fatal(err)
				}
				response := httptest.NewRecorder()
				server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil))
				body := response.Body.String()
				for _, want := range []string{"Acceptance criteria", "AC1", "met", "Spot-check: feature test passed &lt;script&gt;note&lt;/script&gt;", "go test: passed &lt;script&gt;evidence&lt;/script&gt;"} {
					if response.Code != 200 || !strings.Contains(body, want) {
						t.Fatalf("dashboard missing %q", want)
					}
				}
				if strings.Contains(body, "<script>note</script>") {
					t.Fatal("criteria not escaped")
				}
				if _, err := s.Reconcile(context.Background()); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestCriteriaMismatchUsesInvalidResultRetryPath(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			_, runtime, api, _, cfg, r := pairingFlow(t, agent, agent, `printf fixed > feature.txt; `+fixedReport)
			api.issues["owner/repo"][0].Body += "\n## Acceptance criteria\n- Feature works\n"
			cfg.Repositories[0].Implementer.MaxAttempts = 2
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			run := finish(t, s, workflow.NeedsAttention, workflow.Implement)
			if run.LastErrorCode != "phase.result_invalid" || run.Implementer.Attempt != 2 || api.creations != 0 {
				t.Fatalf("missing criteria bypassed retry: %+v", run)
			}
		})
	}
}

func TestBlockedImplementationDisplaysCriteriaBeforeReview(t *testing.T) {
	script := strings.Replace(successfulScript, `"status":"success"`, `"status":"blocked"`, 1)
	script = strings.Replace(script, `"human_action":null`, `"human_action":{"decision":"Choose expected behavior","files":[],"options":[]}`, 1)
	script = strings.Replace(script, `"criteria":[]`, `"criteria":[{"id":"AC1","met":false,"evidence":"Behavior needs a decision <script>blocked</script>"}]`, 1)
	s, runtime, api, _, cfg, _ := localFlow(t, script)
	api.issues["owner/repo"][0].Body += "\n## Acceptance criteria\n- Feature works\n"
	run := finish(t, s, workflow.NeedsAttention, workflow.Implement)
	if run.Review != nil || run.Fix != nil {
		t.Fatal("blocked implementation unexpectedly reviewed")
	}
	server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil))
	for _, want := range []string{"Acceptance criteria", "AC1 · not met", "Behavior needs a decision &lt;script&gt;blocked&lt;/script&gt;"} {
		if response.Code != 200 || !strings.Contains(response.Body.String(), want) {
			t.Fatalf("blocked run dashboard missing %q", want)
		}
	}
}

func TestReviewStartsAfterSuccessfulSetupRetry(t *testing.T) {
	_, rt, api, _, cfg, r := pairingFlow(t, "claude", "claude", `printf fixed > feature.txt; `+fixedReport)
	cfg.Repositories[0].Setup = []string{`if [ ! -f setup-ready ]; then printf ready > setup-ready; exit 7; fi`}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	failed := finish(t, s, workflow.NeedsAttention, workflow.Implement)
	if failed.LastErrorCode != "workspace.setup_failed" {
		t.Fatalf("unexpected setup failure: %s", failed.LastErrorCode)
	}
	if _, err := s.Retry(context.Background(), failed.ID); err != nil {
		t.Fatal(err)
	}
	ready := finish(t, s, workflow.Active, workflow.Review)
	if ready.Implementer.Attempt != 1 || len(ready.ImplementHistory) != 1 || len(ready.SystemSteps) != 2 || ready.SystemSteps[1].Attempt != 2 || ready.SystemSteps[1].Status != "succeeded" {
		t.Fatalf("scenario did not establish setup attempt 2 and implement attempt 1")
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("want one run, got %d", len(runs))
	}
	if runs[0].State != workflow.Active || runs[0].Review == nil {
		t.Fatalf("review did not start: state=%s error=%s message=%s", runs[0].State, runs[0].LastErrorCode, runs[0].LastErrorMessage)
	}

	input, err := os.ReadFile(filepath.Join(rt.Workspace.Root, "runs", ready.ID, "phases", "review-1-1", "input.md"))
	if err != nil || !strings.Contains(string(input), "Added feature") {
		t.Fatalf("review did not receive the implementer report: %v", err)
	}
}

func TestImplementationCriteriaHistorySurvivesRetryAndRestart(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			gate := filepath.Join(t.TempDir(), "release")
			first := strings.Replace(successfulScript, `"criteria":[]`, `"criteria":[{"id":"AC1","met":false,"evidence":"First validation <script>prior</script>"}]`, 1)
			first = strings.Replace(first, `"status":"success"`, `"status":"blocked"`, 1)
			first = strings.Replace(first, `"human_action":null`, `"human_action":{"decision":"Choose expected behavior","files":[],"options":[]}`, 1)
			second := `printf fixed > feature.txt; printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":3,"human_action":null,"status":"success","summary":"Repaired feature","criteria":[{"id":"AC1","met":true,"evidence":"Second validation passed"}]}}'`
			script := `case "$*" in *implement-0-2*) while [ ! -f '` + gate + `' ]; do /bin/sleep 0.02; done; ` + second + `;; *) ` + first + `;; esac`
			_, runtime, api, _, cfg, r := pairingFlow(t, agent, "claude", `printf fixed > feature.txt; `+fixedReport)
			api.issues["owner/repo"][0].Body += "\n## Acceptance criteria\n- Feature works\n"
			replacePhaseScript(t, cfg, workflow.Implement, script)
			t.Cleanup(func() { _ = os.WriteFile(gate, nil, 0600) })
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			blocked := finish(t, s, workflow.NeedsAttention, workflow.Implement)
			if _, err := s.Retry(context.Background(), blocked.ID); err != nil {
				t.Fatal(err)
			}
			waitForWithin(t, 30*time.Second, func() bool {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				runs, err := s.Runs(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				return len(runs) == 1 && runs[0].Implementer != nil && runs[0].Implementer.Attempt == 2 && runs[0].Implementer.Status == "running"
			})
			assertHistory := func(current *app.Runtime, sched *scheduler.Scheduler, completed bool) {
				t.Helper()
				runs, err := sched.Runs(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if len(runs) != 1 || runs[0].Implementer.Attempt != 2 || len(runs[0].ImplementHistory) != 2 {
					t.Fatalf("current snapshot/history mismatch: %+v", runs)
				}
				history := runs[0].ImplementHistory
				if history[0].Attempt != 1 || history[0].Report == nil || history[1].Attempt != 2 {
					t.Fatal("implementation reports are not ordered by attempt")
				}
				if completed {
					if history[1].Report == nil || runs[0].Implementer.Report == nil {
						t.Fatal("completed retry lacks its report")
					}
				} else if history[1].Report != nil || runs[0].Implementer.Report != nil {
					t.Fatal("prior report leaked into active controls")
				}

				server, err := web.NewDashboard(current.Events, current.Scheduler, web.DashboardOptions{DB: current.DB, Workspace: current.Workspace, Scheduler: sched, Config: cfg})
				if err != nil {
					t.Fatal(err)
				}
				for _, hx := range []bool{false, true} {
					req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/runs/"+blocked.ID, nil)
					if hx {
						req.Header.Set("HX-Request", "true")
					}
					response := httptest.NewRecorder()
					server.ServeHTTP(response, req)
					for _, want := range []string{"Implementation · attempt 1", "Implementation · attempt 2", "First validation &lt;script&gt;prior&lt;/script&gt;"} {
						if response.Code != 200 || !strings.Contains(response.Body.String(), want) {
							t.Fatalf("retry history missing %q", want)
						}
					}
					if completed && !strings.Contains(response.Body.String(), "Second validation passed") {
						t.Fatal("latest evidence missing")
					}
				}
			}
			ref, err := s.Watch(context.Background(), blocked.ID)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.StopSession(context.Background(), ref) })
			assertHistory(runtime, s, false)
			root := runtime.Workspace.Root
			if err := runtime.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, err := app.Open(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { restarted.Close() })
			s, err = scheduler.New(cfg, schedulerResources(restarted), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			assertHistory(restarted, s, false)
			if err := os.WriteFile(gate, nil, 0600); err != nil {
				t.Fatal(err)
			}
			finished := finish(t, s, workflow.Active, workflow.Review)
			if finished.Implementer.Attempt != 2 {
				t.Fatal("active attempt changed during restart")
			}
			assertHistory(restarted, s, true)
		})
	}
}
