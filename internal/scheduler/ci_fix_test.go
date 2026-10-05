package scheduler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

const ciFixedReport = `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":1,"status":"success","summary":"Repaired failing CI","responses":[]}}'`

func ciFixScript(fix string) string {
	return `case "$*" in
 *review-*) ` + approvedReview + `;;
 *fix-*) ` + fix + `;;
 *) ` + successfulScript + `;;
 esac`
}

func TestCIFixRequiresAvailableReviewRound(t *testing.T) {
	for _, max := range []int{1, 2} {
		t.Run(string(rune('0'+max)), func(t *testing.T) {
			_, runtime, base, _, cfg, r := localFlow(t, ciFixScript(`printf repaired > feature.txt; `+ciFixedReport))
			cfg.MaxRounds = max
			api := &ciGitHub{fakeGitHub: base, evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "timed_out"}}}}
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: ciBranchGitHub{api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			finish(t, s, workflow.WaitingForCI, workflow.Review)
			run := finish(t, s, workflow.NeedsAttention, workflow.Review)
			if run.LastErrorCode != "review.max_rounds_exceeded" || run.ReviewRound != max || len(run.FixHistory) != max-1 || api.readyCalls != 0 || run.CI.Evidence.Checks[0].Conclusion != "timed_out" {
				t.Fatalf("unreviewable repair launched: %+v", run)
			}
		})
	}
}

func TestCIFixAndReviewerAttemptsDoNotSpendExtraRounds(t *testing.T) {
	fix := `case "$*" in *fix-1-1*) exit 1;; esac
printf repaired > feature.txt; ` + ciFixedReport
	script := strings.Replace(ciFixScript(fix), "*review-*)", "*review-2-1*) exit 1;;\n *review-*)", 1)
	_, runtime, base, _, cfg, r := localFlow(t, script)
	cfg.Repositories[0].Implementer.MaxAttempts = 2
	cfg.Repositories[0].Reviewer.MaxAttempts = 2
	api := &ciGitHub{fakeGitHub: base, evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "failure"}}}}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: ciBranchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	finish(t, s, workflow.WaitingForCI, workflow.Review)
	finish(t, s, workflow.Active, workflow.Fix)
	api.evidence = ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "success"}}}
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if run.ReviewRound != 2 || run.Fix.Attempt != 2 || run.Review.Attempt != 2 {
		t.Fatalf("attempts spent extra rounds: %+v", run)
	}
}

func TestCINonCodeOutcomesNeverLaunchRepair(t *testing.T) {
	for _, mode := range []string{"cancellation with failure", "action with timeout", "wait deadline", "unreadable failed evidence", "expired failure", "expired terminal timeout", "failure query crosses deadline", "PR reread crosses deadline"} {
		t.Run(mode, func(t *testing.T) {
			_, runtime, base, _, cfg, r := localFlow(t, successfulScript)
			now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
			api := &ciGitHub{fakeGitHub: base, evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "failure"}}}}
			delayRead := false
			boundary := ciDeadlineGitHub{ciGitHub: api, beforeRead: func() {
				if delayRead {
					now = now.Add(time.Hour)
					delayRead = false
				}
			}}
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: boundary, Runner: r, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			run := finish(t, s, workflow.WaitingForCI, workflow.Review)
			code := "ci.action_required"
			switch mode {
			case "cancellation with failure":
				api.evidence.Checks = append(api.evidence.Checks, ci.Check{Name: "canceled", Source: "check", Status: "completed", Conclusion: "cancelled"})
			case "action with timeout":
				api.evidence.Checks = []ci.Check{{Name: "action", Source: "check", Status: "completed", Conclusion: "action_required"}, {Name: "timeout", Source: "check", Status: "completed", Conclusion: "timed_out"}}
			case "wait deadline":
				api.evidence.Checks[0].Status = "queued"
				api.evidence.Checks[0].Conclusion = ""
				now = now.Add(time.Hour)
				code = "ci.wait_timeout"
			case "unreadable failed evidence":
				api.queryError = os.ErrPermission
				code = "ci.wait_timeout"
			case "expired failure", "expired terminal timeout":
				now = now.Add(time.Hour)
				code = "ci.wait_timeout"
				if mode == "expired terminal timeout" {
					api.evidence.Checks[0].Conclusion = "timed_out"
				}
			case "failure query crosses deadline":
				api.afterEvidence = func() { now = now.Add(time.Hour) }
				code = "ci.wait_timeout"
			case "PR reread crosses deadline":
				api.afterEvidence = func() { delayRead = true }
				code = "ci.wait_timeout"
			}
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if mode == "unreadable failed evidence" {
				waiting, _ := runtime.Workflow.Get(context.Background(), run.ID)
				if waiting.State != workflow.WaitingForCI || waiting.CI.QueryError == "" || !waiting.CI.Deadline.Equal(run.CI.Deadline) {
					t.Fatal("unreadable failure must retain bounded wait")
				}
				now = now.Add(time.Hour)
			}
			for range 2 {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			got, _ := runtime.Workflow.Get(context.Background(), run.ID)
			if got.State != workflow.NeedsAttention || got.LastErrorCode != code || len(got.FixHistory) != 0 || api.readyCalls != 0 {
				t.Fatalf("non-code CI outcome launched repair: %+v", got)
			}
		})
	}
}

type ciDeadlineGitHub struct {
	*ciGitHub
	beforeRead func()
}

func (f ciDeadlineGitHub) GetPullRequest(ctx context.Context, repo string, n int) (*github.PullRequest, error) {
	f.beforeRead()
	return f.fakeGitHub.GetPullRequest(ctx, repo, n)
}

func TestCIFailureRequiresFixThenIndependentReview(t *testing.T) {
	for _, conclusion := range []string{"failure", "timed_out"} {
		t.Run(conclusion, func(t *testing.T) {
			_, runtime, base, remote, cfg, r := localFlow(t, ciFixScript(`printf repaired > feature.txt; `+ciFixedReport))
			api := &ciGitHub{fakeGitHub: base, applyReady: true, evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: conclusion, URL: "https://example.com/build", Excerpt: "feature.go:12: undefined name"}}}}
			cause := "ci.check_failed"
			if conclusion == "timed_out" {
				cause = "ci.check_timed_out"
			}
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: ciBranchGitHub{api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			run := finish(t, s, workflow.WaitingForCI, workflow.Review)
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			fix, err := runtime.Workflow.Get(context.Background(), run.ID)
			if err != nil || fix.State != workflow.Active || fix.Phase != workflow.Fix || fix.ApprovedSHA != "" {
				t.Fatalf("CI did not enter repair with approval cleared: %+v %v", fix, err)
			}
			// A passing check on the old head cannot bypass the new independent review.
			api.evidence = ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "success"}}}
			reviewing := finish(t, s, workflow.Active, workflow.Review)
			if reviewing.ReviewRound != 2 || reviewing.ApprovedSHA != "" || api.readyCalls != 0 {
				t.Fatalf("repair skipped review: %+v", reviewing)
			}
			input, err := os.ReadFile(filepath.Join(runtime.Workspace.Root, "runs", run.ID, "phases", "fix-1-1", "input.md"))
			for _, want := range []string{"build", conclusion, "feature.go:12: undefined name", "https://example.com/build", "responses: []"} {
				if err != nil || !strings.Contains(string(input), want) {
					t.Fatalf("fix input missing %q: %s %v", want, input, err)
				}
			}
			approved := finish(t, s, workflow.WaitingForCI, workflow.Review)
			if approved.ApprovedSHA == run.ApprovedSHA || approved.ApprovedSHA != gitCommand(t, remote, "rev-parse", "mergeyard/issue-7") || approved.Fix == nil || !approved.Fix.Pushed || len(approved.Fix.Report.Responses) != 0 {
				t.Fatalf("new approval or CI-only report missing: %+v", approved)
			}
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			ready, _ := runtime.Workflow.Get(context.Background(), run.ID)
			if ready.State != workflow.ReadyToMerge || api.readyCalls != 1 {
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
				server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331"+path, nil))
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
	for _, tc := range []struct {
		name, fix, code string
		attempts        int
	}{
		{"invented findings", fixedReport, "phase.result_invalid", 2},
		{"invalid output", `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":1,"status":"success","summary":"Missing"}}'`, "phase.result_invalid", 2},
		{"no code changes", ciFixedReport, "phase.result_invalid", 2},
		{"blocked", strings.Replace(ciFixedReport, `"status":"success"`, `"status":"blocked"`, 1), "phase.blocked", 1},
		{"failed", "exit 1", "phase.retries_exhausted", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, runtime, base, remote, cfg, r := localFlow(t, ciFixScript(tc.fix))
			cfg.Repositories[0].Implementer.MaxAttempts = 2
			api := &ciGitHub{fakeGitHub: base, evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "failure"}}}}
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: ciBranchGitHub{api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			finish(t, s, workflow.WaitingForCI, workflow.Review)
			run := finish(t, s, workflow.NeedsAttention, workflow.Fix)
			if run.LastErrorCode != tc.code || run.ReviewRound != 1 || run.ApprovedSHA != "" || run.Fix.Attempt != tc.attempts || len(run.FixHistory) != tc.attempts || api.readyCalls != 0 {
				t.Fatalf("invalid fix accepted or limit ignored: %+v fix=%+v", run, run.Fix)
			}
			if gitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7") != "1" {
				t.Fatal("invalid repair published")
			}
		})
	}
}
