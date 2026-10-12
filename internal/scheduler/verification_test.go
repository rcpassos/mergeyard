package scheduler_test

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
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func (f *fakeGitHub) UpdatePullRequest(ctx context.Context, repo string, n int, content github.PullRequestContent) (*github.PullRequest, error) {
	pr, err := f.GetPullRequest(ctx, repo, n)
	if err != nil {
		return nil, err
	}
	body, err := github.UpdatePullRequestBody(pr.Body, content)
	if err != nil {
		return nil, err
	}
	pr.Body = body
	return pr, nil
}

func TestVerificationFlowsThroughPRReviewAndDashboard(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			fix := `printf fixed > feature.txt; ` + fixedReport
			s, rt, api, _, cfg, _ := pairingFlow(t, agent, agent, fix)
			script := loopScript(fix)
			implement := strings.Replace(successfulScript, `"verification":[]`, `"verification":[{"command":"implement test","outcome":"passed","excerpt":"Initial verification"}]`, 1)
			repaired := strings.Replace(fixedReport, `"verification":[]`, `"verification":[{"command":"fix test","outcome":"failed","excerpt":"Fix verification <details>@someone"},{"command":"build","outcome":"skipped","excerpt":null}]`, 1)
			script = strings.Replace(script, successfulScript, implement, 1)
			script = strings.Replace(script, fixedReport, repaired, 1)
			replacePhaseScript(t, cfg, workflow.Implement, script)
			replacePhaseScript(t, cfg, workflow.Review, script)
			first := finish(t, s, workflow.Active, workflow.Review)
			pr, err := api.GetPullRequest(context.Background(), first.Repository, first.PRNumber)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(pr.Body, "Initial verification") {
				t.Fatal("PR missing implement verification")
			}
			pr.Body = "Human introduction\n" + pr.Body + "\nHuman checklist"
			run := finish(t, s, workflow.WaitingForCI, workflow.Review)
			if len(run.ImplementHistory[0].Report.Verification) != 1 || len(run.Fix.Report.Verification) != 2 {
				t.Fatal("verification missing from saved phase")
			}
			for _, tc := range []struct{ phase, want string }{{"review-1-1", "Initial verification"}, {"review-2-1", "Fix verification"}} {
				input, err := os.ReadFile(filepath.Join(rt.Workspace.Root, "runs", run.ID, "phases", tc.phase, "input.md"))
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
			server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil))
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

type evidenceGitHub struct {
	branchGitHub
	offline bool
	updates int
}

func (f *evidenceGitHub) UpdatePullRequest(ctx context.Context, repo string, n int, content github.PullRequestContent) (*github.PullRequest, error) {
	f.updates++
	if f.offline {
		return nil, &fault.Error{Code: "github.unavailable", Message: "Offline"}
	}
	return f.fakeGitHub.UpdatePullRequest(ctx, repo, n, content)
}

func TestVerificationBodyRetriesLatestReportOnRestartWithCommentsDisabled(t *testing.T) {
	_, rt, api, _, cfg, r := pairingFlow(t, "claude", "claude", `printf fixed > feature.txt; `+fixedReport)
	cfg.PRComments = false
	publisher := &evidenceGitHub{branchGitHub: branchGitHub{api}, offline: true}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: publisher, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
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
	publisher.offline = false
	s, err = scheduler.New(cfg, schedulerResources(restarted), scheduler.Dependencies{GitHub: publisher, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pr.Body, "Fixed <script>bug</script>") || !strings.Contains(pr.Body, "no verification reported") || !strings.HasPrefix(pr.Body, "Human introduction\n") || !strings.HasSuffix(pr.Body, "\nHuman checklist") {
		t.Fatalf("latest fix body lost on restart: %s", pr.Body)
	}
	delivered := publisher.updates
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if publisher.updates != delivered {
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
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/runs/"+runs[0].ID, nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), "no verification reported") {
		t.Fatal("empty verification not shown on dashboard")
	}
}

func TestReviewRetryKeepsVerificationFromAlreadyAdjudicatedFix(t *testing.T) {
	fix := strings.Replace(fixedReport, `"verification":[]`, `"verification":[{"command":"fix test","outcome":"passed","excerpt":"Latest fix verification"}]`, 1)
	_, rt, api, _, cfg, r := pairingFlow(t, "claude", "claude", `printf fixed > feature.txt; `+fix)
	publisher := &evidenceGitHub{branchGitHub: branchGitHub{api}, offline: true}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: publisher, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	before := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if _, err := rt.Workflow.Transition(context.Background(), before.ID, workflow.Request{Trigger: workflow.OperationFailed, Failure: &fault.Error{Code: "ci.wait_timeout", Message: "CI wait expired"}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(rt.Workspace.Root, "worktrees", "owner-repo", before.ID)
	if err := os.WriteFile(filepath.Join(path, "feature.txt"), []byte("Human change after accepted fix"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, path, "add", "feature.txt")
	gitCommand(t, path, "commit", "-m", "Human change")
	gitCommand(t, path, "push", "origin", "HEAD")
	replacePhaseScript(t, cfg, workflow.Review, reviewScript(approvedReview))
	if _, err := s.Retry(context.Background(), before.ID); err != nil {
		t.Fatal(err)
	}
	after := finish(t, s, workflow.WaitingForCI, workflow.Review)
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
