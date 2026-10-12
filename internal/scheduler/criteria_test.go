package scheduler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
