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

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestCIFixAndReviewerAttemptsDoNotSpendExtraRounds(t *testing.T) {
	t.Parallel()

	fix := `case "$*" in *fix-1-1*) exit 1;; esac
printf repaired > feature.txt; ` + testkit.CiFixedReport
	script := strings.Replace(testkit.CiFixScript(fix), "*review-*)", "*review-2-1*) exit 1;;\n *review-*)", 1)
	_, runtime, base, _, cfg, r := testkit.TmuxFlow(t, script)
	cfg.Repositories[0].Implementer.MaxAttempts = 2
	cfg.Repositories[0].Reviewer.MaxAttempts = 2
	api := &testkit.CiGitHub{FakeGitHub: base, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "failure"}}}}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.CiBranchGitHub{CiGitHub: api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	testkit.Finish(t, s, workflow.Active, workflow.Fix)
	api.Evidence = ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "success"}}}
	run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	if run.ReviewRound != 2 || run.Fix.Attempt != 2 || run.Review.Attempt != 2 {
		t.Fatalf("attempts spent extra rounds: %+v", run)
	}
}

func TestCIFailureRequiresFixThenIndependentReview(t *testing.T) {
	t.Parallel()

	for _, conclusion := range []string{"failure", "timed_out"} {
		t.Run(conclusion, func(t *testing.T) {
			t.Parallel()

			_, runtime, base, remote, cfg, r := testkit.TmuxFlow(t, testkit.CiFixScript(`printf repaired > feature.txt; `+testkit.CiFixedReport))
			api := &testkit.CiGitHub{FakeGitHub: base, ApplyReady: true, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: conclusion, URL: "https://example.com/build", Excerpt: "feature.go:12: undefined name"}}}}
			cause := "ci.check_failed"
			if conclusion == "timed_out" {
				cause = "ci.check_timed_out"
			}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.CiBranchGitHub{CiGitHub: api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			fix, err := runtime.Workflow.Get(context.Background(), run.ID)
			if err != nil || fix.State != workflow.Active || fix.Phase != workflow.Fix || fix.ApprovedSHA != "" {
				t.Fatalf("CI did not enter repair with approval cleared: %+v %v", fix, err)
			}
			// A passing check on the old head cannot bypass the new independent review.
			api.Evidence = ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "success"}}}
			reviewing := testkit.Finish(t, s, workflow.Active, workflow.Review)
			if reviewing.ReviewRound != 2 || reviewing.ApprovedSHA != "" || api.ReadyCalls != 0 {
				t.Fatalf("repair skipped review: %+v", reviewing)
			}
			input, err := os.ReadFile(filepath.Join(runtime.Workspace.Root, "runs", run.ID, "phases", "fix-1-1", "input.md"))
			for _, want := range []string{"build", conclusion, "feature.go:12: undefined name", "https://example.com/build", "responses: []"} {
				if err != nil || !strings.Contains(string(input), want) {
					t.Fatalf("fix input missing %q: %s %v", want, input, err)
				}
			}
			approved := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
			if approved.ApprovedSHA == run.ApprovedSHA || approved.ApprovedSHA != testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7") || approved.Fix == nil || !approved.Fix.Pushed || len(approved.Fix.Report.Responses) != 0 {
				t.Fatalf("new approval or CI-only report missing: %+v", approved)
			}
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			ready, _ := runtime.Workflow.Get(context.Background(), run.ID)
			if ready.State != workflow.ReadyToMerge || api.ReadyCalls != 1 {
				t.Fatalf("not ready after reviewed repair: %+v", ready)
			}
			// Failure evidence must outlive the new successful CI snapshot.
			encoded, err := json.Marshal(ready.Fix)
			if err != nil || !strings.Contains(string(encoded), cause) || !strings.Contains(string(encoded), "https://example.com/build") {
				t.Fatalf("repair cause or diagnostics lost: %s %v", encoded, err)
			}
			input, err = os.ReadFile(filepath.Join(runtime.Workspace.Root, "runs", run.ID, "phases", "review-2-1", "input.md"))
			for _, want := range []string{cause, "https://example.com/build", "Repaired failing CI", "feature.go:12: undefined name"} {
				if err != nil || !strings.Contains(string(input), want) {
					t.Fatalf("next review missing %q: %s %v", want, input, err)
				}
			}
			server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"/runs/" + run.ID, "/api/status"} {
				response := httptest.NewRecorder()
				request := testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331"+path, nil)
				testkit.SignAPIRequest(t, runtime.Workspace.Root, request)
				server.ServeHTTP(response, request)
				for _, want := range []string{cause, "https://example.com/build", "Repaired failing CI", "feature.go:12: undefined name"} {
					if response.Code != 200 || !strings.Contains(response.Body.String(), want) {
						t.Fatalf("%s missing %q", path, want)
					}
				}
			}
		})
	}
}

func TestCIFixInvalidOutputObeysImplementerAttemptLimit(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, fix, Code string
		attempts        int
	}{
		{"invented findings", testkit.FixedReport, "phase.result_invalid", 2},
		{"invalid output", `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":4,"verification":[],"criteria":[],"human_action":null,"status":"success","summary":"Missing"}}'`, "phase.result_invalid", 2},
		{"no code changes", testkit.CiFixedReport, "phase.result_invalid", 2},
		{"blocked", testkit.BlockedReport(testkit.CiFixedReport), "phase.blocked", 1},
		{"failed", "exit 1", "phase.retries_exhausted", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, runtime, base, remote, cfg, r := testkit.TmuxFlow(t, testkit.CiFixScript(tc.fix))
			cfg.Repositories[0].Implementer.MaxAttempts = 2
			api := &testkit.CiGitHub{FakeGitHub: base, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "failure"}}}}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.CiBranchGitHub{CiGitHub: api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
			run := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Fix)
			if run.LastErrorCode != tc.Code || run.ReviewRound != 1 || run.ApprovedSHA != "" || run.Fix.Attempt != tc.attempts || len(run.FixHistory) != tc.attempts || api.ReadyCalls != 0 {
				t.Fatalf("invalid fix accepted or limit ignored: %+v fix=%+v", run, run.Fix)
			}
			if testkit.GitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7") != "1" {
				t.Fatal("invalid repair published")
			}
		})
	}
}
