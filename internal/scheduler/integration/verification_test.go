package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestVerificationFlowsThroughPRReviewAndDashboard(t *testing.T) {
	t.Parallel()

	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			t.Parallel()

			fix := `printf fixed > feature.txt; ` + testkit.FixedReport
			s, rt, api, _, cfg, _ := testkit.TmuxPairingFlow(t, agent, agent, fix)
			script := testkit.LoopScript(fix)
			implement := strings.Replace(testkit.SuccessfulScript, `"verification":[]`, `"verification":[{"command":"implement test","outcome":"passed","excerpt":"Initial verification"}]`, 1)
			repaired := strings.Replace(testkit.FixedReport, `"verification":[]`, `"verification":[{"command":"fix test","outcome":"failed","excerpt":"Fix verification <details>@someone"},{"command":"build","outcome":"skipped","excerpt":null}]`, 1)
			script = strings.Replace(script, testkit.SuccessfulScript, implement, 1)
			script = strings.Replace(script, testkit.FixedReport, repaired, 1)
			testkit.ReplacePhaseScript(t, cfg, workflow.Implement, script)
			testkit.ReplacePhaseScript(t, cfg, workflow.Review, script)
			first := testkit.Finish(t, s, workflow.Active, workflow.Review)
			pr, err := api.GetPullRequest(context.Background(), first.Repository, first.PRNumber)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(pr.Body, "Initial verification") {
				t.Fatal("PR missing implement verification")
			}
			pr.Body = "Human introduction\n" + pr.Body + "\nHuman checklist"
			run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
			if len(run.ImplementHistory[0].Report.Verification) != 1 || len(run.Fix.Report.Verification) != 2 {
				t.Fatal("verification missing from saved phase")
			}
			for _, tc := range []struct{ Phase, want string }{{"review-1-1", "Initial verification"}, {"review-2-1", "Fix verification"}} {
				input, err := os.ReadFile(filepath.Join(rt.Workspace.Root, "runs", run.ID, "phases", tc.Phase, "input.md"))
				if err != nil || !strings.Contains(string(input), tc.want) {
					t.Fatalf("review missing latest evidence: %v", err)
				}
			}
			if !strings.HasPrefix(pr.Body, "Human introduction\n") || !strings.HasSuffix(pr.Body, "\nHuman checklist") || !strings.Contains(pr.Body, "Fix verification") || strings.Contains(pr.Body, "Initial verification") || strings.Contains(pr.Body, "@someone") {
				t.Fatalf("PR body did not replace latest evidence safely: %s", pr.Body)
			}
			server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil))
			for _, want := range []string{"Verification evidence", "implement test", "Initial verification", "fix test", "failed", "skipped", "Fix verification &lt;details&gt;@someone"} {
				if response.Code != 200 || !strings.Contains(response.Body.String(), want) {
					t.Fatalf("dashboard missing %q", want)
				}
			}
			before := pr.Body
			if _, err := s.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			if pr.Body != before {
				t.Fatal("reconciliation changed already-published body")
			}
		})
	}
}

func TestVerificationBodyRetriesLatestReportOnRestartWithCommentsDisabled(t *testing.T) {
	t.Parallel()

	_, rt, api, _, cfg, r := testkit.TmuxPairingFlow(t, "claude", "claude", `printf fixed > feature.txt; `+testkit.FixedReport)
	cfg.PRComments = false
	publisher := &testkit.EvidenceGitHub{BranchGitHub: testkit.BranchGitHub{FakeGitHub: api}, Offline: true}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: publisher, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	pr, err := api.GetPullRequest(context.Background(), run.Repository, run.PRNumber)
	if err != nil {
		t.Fatal(err)
	}
	pr.Body = "Human introduction\n" + pr.Body + "\nHuman checklist"
	root := rt.Workspace.Root
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := app.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Close() })
	publisher.Offline = false
	// The deferred attempt backs off; age it so the restart can retry.
	if _, err := restarted.DB.Exec(`UPDATE verification_attempts SET last_attempt_at='2000-01-01T00:00:00Z' WHERE run_id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(restarted), scheduler.Dependencies{GitHub: publisher, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pr.Body, "Fixed <script>bug</script>") || !strings.Contains(pr.Body, "no verification reported") || !strings.HasPrefix(pr.Body, "Human introduction\n") || !strings.HasSuffix(pr.Body, "\nHuman checklist") {
		t.Fatalf("latest fix body lost on restart: %s", pr.Body)
	}
	delivered := publisher.Updates
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if publisher.Updates != delivered {
		t.Fatal("receipt did not prevent duplicate delivery")
	}
	runs, err := s.Runs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	server, err := web.NewDashboard(restarted.Events, restarted.Scheduler, web.DashboardOptions{DB: restarted.DB, Workspace: restarted.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/"+runs[0].ID, nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), "no verification reported") {
		t.Fatal("empty verification not shown on dashboard")
	}
}

func TestReviewRetryKeepsVerificationFromAlreadyAdjudicatedFix(t *testing.T) {
	t.Parallel()

	fix := strings.Replace(testkit.FixedReport, `"verification":[]`, `"verification":[{"command":"fix test","outcome":"passed","excerpt":"Latest fix verification"}]`, 1)
	_, rt, api, _, cfg, r := testkit.TmuxPairingFlow(t, "claude", "claude", `printf fixed > feature.txt; `+fix)
	publisher := &testkit.EvidenceGitHub{BranchGitHub: testkit.BranchGitHub{FakeGitHub: api}, Offline: true}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: publisher, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	before := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	if _, err := rt.Workflow.Transition(context.Background(), before.ID, workflow.Request{Trigger: workflow.OperationFailed, Failure: &fault.Error{Code: "ci.wait_timeout", Message: "CI wait expired"}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(rt.Workspace.Root, "worktrees", "owner-repo", before.ID)
	if err := os.WriteFile(filepath.Join(path, "feature.txt"), []byte("Human change after accepted fix"), 0600); err != nil {
		t.Fatal(err)
	}
	testkit.GitCommand(t, path, "add", "feature.txt")
	testkit.GitCommand(t, path, "commit", "-m", "Human change")
	testkit.GitCommand(t, path, "push", "origin", "HEAD")
	testkit.ReplacePhaseScript(t, cfg, workflow.Review, testkit.ReviewScript(testkit.ApprovedReview))
	if _, err := s.Retry(context.Background(), before.ID); err != nil {
		t.Fatal(err)
	}
	after := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	if after.ReviewRound != 2 || after.Review.Attempt != 2 {
		t.Fatal("did not retry accepted round")
	}
	input, err := os.ReadFile(filepath.Join(rt.Workspace.Root, "runs", after.ID, "phases", "review-2-2", "input.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(input), "Latest fix verification") {
		t.Fatal("adjudicated fix verification lost from review retry")
	}
	if strings.Contains(string(input), "Changed source") {
		t.Fatal("already adjudicated fix responses leaked into retry")
	}
}

func TestNonSuccessImplementVerificationSurvivesManualPRHandback(t *testing.T) {

	for _, status := range []string{"blocked", "failed"} {
		t.Run(status, func(t *testing.T) {

			ctx := context.Background()
			report := testkit.BlockedReport(testkit.SuccessfulScript)
			if status == "failed" {
				report = strings.Replace(testkit.SuccessfulScript, `"status":"success"`, `"status":"failed"`, 1)
			}
			script := strings.Replace(report, `"verification":[]`, `"verification":[{"command":"non-success implement test","outcome":"failed","excerpt":"non-success implement evidence marker"}]`, 1)
			s, runtime, api, remote, cfg, r := testkit.TmuxFlow(t, "sleep 0.2; "+script)
			before := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
			if before.Implementer.Report == nil || len(before.Implementer.Report.Verification) != 1 {
				t.Fatal("fixture missing saved non-success evidence")
			}
			// Stopping or rejecting later rewrites the attempt's error. The
			// evidence must follow the eligibility frozen with the report.
			if _, err := runtime.DB.ExecContext(ctx, "UPDATE phase_attempts SET error='phase.canceled: Stopped by operator' WHERE run_id=? AND phase='implement' AND step=''", before.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Takeover(ctx, before.ID); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", before.ID)
			testkit.GitCommand(t, path, "add", ".")
			testkit.GitCommand(t, path, "commit", "-m", "manual PR work")
			testkit.GitCommand(t, path, "push", "origin", "HEAD:refs/heads/mergeyard/issue-7")
			head := testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7")
			pr := &github.PullRequest{Number: 88, State: github.Open, URL: "https://github.com/owner/repo/pull/88", Body: "human PR body", Draft: true}
			pr.Head.Ref = "mergeyard/issue-7"
			pr.Head.Repo.FullName = "owner/repo"
			pr.Head.SHA = head
			pr.Base.Ref = "main"
			api.Prs = map[string]*github.PullRequest{"mergeyard/issue-7": pr}
			// Refresh PR head from the fixture remote during handback and review.
			var err error
			s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			selected, err := s.Handback(ctx, before.ID)
			if err != nil || selected.Phase != workflow.Review {
				t.Fatalf("handback selection: %+v %v", selected, err)
			}
			after := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
			input, err := os.ReadFile(filepath.Join(runtime.Workspace.Root, "runs", after.ID, "phases", "review-1-1", "input.md"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(pr.Body, "non-success implement evidence marker") {
				t.Fatal("publication fixture did not eventually publish non-success evidence")
			}
			if !strings.Contains(string(input), "non-success implement evidence marker") {
				t.Fatal("review omitted latest saved non-success implement evidence; PR publication occurs only after review starts")
			}
		})
	}
}
