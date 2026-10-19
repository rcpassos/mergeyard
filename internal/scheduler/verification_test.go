package scheduler_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/config"
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
			server.ServeHTTP(response, dashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil))
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
	code    string
	updates int
}

func (f *evidenceGitHub) UpdatePullRequest(ctx context.Context, repo string, n int, content github.PullRequestContent) (*github.PullRequest, error) {
	f.updates++
	if f.offline {
		code := f.code
		if code == "" {
			code = "github.unavailable"
		}
		return nil, &fault.Error{Code: code, Message: "Offline"}
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
	server.ServeHTTP(response, dashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/"+runs[0].ID, nil))
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

func TestNonSuccessImplementVerificationSurvivesManualPRHandback(t *testing.T) {
	for _, status := range []string{"blocked", "failed"} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			report := blockedReport(successfulScript)
			if status == "failed" {
				report = strings.Replace(successfulScript, `"status":"success"`, `"status":"failed"`, 1)
			}
			script := strings.Replace(report, `"verification":[]`, `"verification":[{"command":"non-success implement test","outcome":"failed","excerpt":"non-success implement evidence marker"}]`, 1)
			s, runtime, api, remote, cfg, r := localFlow(t, "sleep 0.2; "+script)
			before := finish(t, s, workflow.NeedsAttention, workflow.Implement)
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
			gitCommand(t, path, "add", ".")
			gitCommand(t, path, "commit", "-m", "manual PR work")
			gitCommand(t, path, "push", "origin", "HEAD:refs/heads/mergeyard/issue-7")
			head := gitCommand(t, remote, "rev-parse", "mergeyard/issue-7")
			pr := &github.PullRequest{Number: 88, State: github.Open, URL: "https://github.com/owner/repo/pull/88", Body: "human PR body", Draft: true}
			pr.Head.Ref = "mergeyard/issue-7"
			pr.Head.Repo.FullName = "owner/repo"
			pr.Head.SHA = head
			pr.Base.Ref = "main"
			api.prs = map[string]*github.PullRequest{"mergeyard/issue-7": pr}
			// Refresh PR head from the fixture remote during handback and review.
			var err error
			s, err = scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			selected, err := s.Handback(ctx, before.ID)
			if err != nil || selected.Phase != workflow.Review {
				t.Fatalf("handback selection: %+v %v", selected, err)
			}
			after := finish(t, s, workflow.WaitingForCI, workflow.Review)
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

type stalledEvidenceGitHub struct {
	branchGitHub
	bounded bool
}

func (f *stalledEvidenceGitHub) UpdatePullRequest(ctx context.Context, _ string, _ int, _ github.PullRequestContent) (*github.PullRequest, error) {
	deadline, ok := ctx.Deadline()
	f.bounded = ok && time.Until(deadline) <= 5100*time.Millisecond
	if !f.bounded {
		return nil, context.DeadlineExceeded
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestStalledVerificationPublicationReleasesReconciliationAndRetries(t *testing.T) {
	_, rt, api, _, cfg, r := localFlow(t, successfulScript)
	cfg.PRComments = false
	offline := &evidenceGitHub{branchGitHub: branchGitHub{api}, offline: true}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: offline, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
	stalled := &stalledEvidenceGitHub{branchGitHub: branchGitHub{api}}
	s, err = scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: stalled, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	started := time.Now()
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if !stalled.bounded || time.Since(started) > 7*time.Second || ctx.Err() != nil {
		t.Fatal("optional publication was not bounded independently of caller")
	}
	history, err := rt.Events.History(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	warning := false
	for _, e := range history {
		if e.RunID == run.ID && e.Type == "verification.warning" {
			warning = true
		}
	}
	if !warning {
		t.Fatal("publication timeout warning was not saved")
	}
	healthy := &evidenceGitHub{branchGitHub: branchGitHub{api}}
	s, err = scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: healthy, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if healthy.updates != 1 {
		t.Fatal("timed out evidence was not retained for retry")
	}
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if healthy.updates != 1 {
		t.Fatal("successful retry receipt was not saved")
	}
}

// seedVerificationRun inserts a run and its latest implement evidence without
// launching an agent, so publication reconciliation owns it immediately.
func seedVerificationRun(t *testing.T, rt *app.Runtime, id, state string, issue, pr int) {
	t.Helper()
	ctx := context.Background()
	if _, err := rt.DB.ExecContext(ctx, `INSERT INTO runs(id,repository,issue_number,state,pr_number) VALUES(?,'owner/repo',?,?,?)`, id, issue, state, pr); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.DB.ExecContext(ctx, `INSERT INTO scheduler_runs(run_id,issue_json) VALUES(?,?)`, id, fmt.Sprintf(`{"number":%d,"title":"Feature"}`, issue)); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.DB.ExecContext(ctx, `INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status,step,report_json,report_eligible) VALUES(?,?,'implement','implementer',0,1,'claude','succeeded','',?,1)`, id+"-attempt", id,
		`{"schema_version":4,"criteria":[],"human_action":null,"status":"success","summary":"Valid evidence","verification":[{"command":"test","outcome":"passed","excerpt":null}]}`); err != nil {
		t.Fatal(err)
	}
}

func countEvents(t *testing.T, rt *app.Runtime, query string, args ...any) int {
	t.Helper()
	var count int
	if err := rt.DB.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// failingVerificationFlow builds a runtime whose PR body updates fail with code.
func failingVerificationFlow(t *testing.T, code string) (*scheduler.Scheduler, *app.Runtime, *evidenceGitHub, config.Config) {
	t.Helper()
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}, prs: map[string]*github.PullRequest{
		"feature": {Number: 101, State: github.Open, Body: "human body"},
	}, head: func(string) string { return "" }}
	_, rt := fixture(t, api)
	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
	if err != nil {
		t.Fatal(err)
	}
	publisher := &evidenceGitHub{branchGitHub: branchGitHub{api}, offline: true, code: code}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: publisher})
	if err != nil {
		t.Fatal(err)
	}
	return s, rt, publisher, cfg
}

func TestFailingVerificationPublicationKeepsAttemptsBounded(t *testing.T) {
	s, rt, publisher, cfg := failingVerificationFlow(t, "github.unavailable")
	ctx := context.Background()
	// MANUAL keeps the run open so every reconciliation retries, like a live
	// run whose PR updates keep failing while it waits for a human.
	seedVerificationRun(t, rt, "probe-manual", "MANUAL", 7, 101)
	baseline := countEvents(t, rt, `SELECT count(*) FROM events`)
	growth := []int{}
	for range 3 {
		if _, err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		growth = append(growth, countEvents(t, rt, `SELECT count(*) FROM events`))
	}
	if publisher.updates != 3 {
		t.Fatalf("live run publication attempts = %d, want one per reconciliation", publisher.updates)
	}
	if attempts := countEvents(t, rt, `SELECT count(*) FROM events WHERE type='verification.attempted'`); attempts != 0 {
		t.Fatalf("publication attempts saved %d event rows", attempts)
	}
	if growth[0] != baseline+1 || growth[1] != growth[0] || growth[2] != growth[0] {
		t.Fatalf("event rows kept growing: baseline %d, after reconciliations %v", baseline, growth)
	}
	if warnings := countEvents(t, rt, `SELECT count(*) FROM events WHERE run_id=? AND type='verification.warning'`, "probe-manual"); warnings != 1 {
		t.Fatalf("warnings = %d, want one per source and code", warnings)
	}
	var warning string
	if err := rt.DB.QueryRow(`SELECT json_extract(payload_json,'$.message') FROM events WHERE run_id=? AND type='verification.warning'`, "probe-manual").Scan(&warning); err != nil || !strings.Contains(warning, "will retry") {
		t.Fatalf("live-run warning = %q, %v", warning, err)
	}
	var attempts int
	if err := rt.DB.QueryRow(`SELECT attempts FROM verification_attempts WHERE run_id=?`, "probe-manual").Scan(&attempts); err != nil || attempts != 3 {
		t.Fatalf("durable attempt record = %d, %v", attempts, err)
	}
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, dashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/probe-manual", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "verification.warning") || !strings.Contains(response.Body.String(), "will retry") {
		t.Fatalf("run page lost the publication warning: %d", response.Code)
	}
}

func TestFinishedRunStopsRetryingUnresolvableVerificationFailure(t *testing.T) {
	s, rt, publisher, _ := failingVerificationFlow(t, "github.not_found")
	ctx := context.Background()
	seedVerificationRun(t, rt, "probe-stopped", "STOPPED", 7, 101)
	baseline := countEvents(t, rt, `SELECT count(*) FROM events`)
	growth := []int{}
	for range 3 {
		if _, err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		growth = append(growth, countEvents(t, rt, `SELECT count(*) FROM events`))
	}
	if publisher.updates != 1 {
		t.Fatalf("finished run was retried %d times, want a single delivery attempt", publisher.updates)
	}
	if growth[0] != baseline+1 || growth[1] != growth[0] || growth[2] != growth[0] {
		t.Fatalf("finished run kept growing event rows: baseline %d, after reconciliations %v", baseline, growth)
	}
	if warnings := countEvents(t, rt, `SELECT count(*) FROM events WHERE run_id=? AND type='verification.warning'`, "probe-stopped"); warnings != 1 {
		t.Fatalf("warnings = %d, want one per source and code", warnings)
	}
	var warning string
	if err := rt.DB.QueryRow(`SELECT json_extract(payload_json,'$.message') FROM events WHERE run_id=? AND type='verification.warning'`, "probe-stopped").Scan(&warning); err != nil || !strings.Contains(warning, "cannot update this PR") {
		t.Fatalf("finished-run warning = %q, %v", warning, err)
	}
	var failed bool
	if err := rt.DB.QueryRow(`SELECT failed FROM verification_attempts WHERE run_id=?`, "probe-stopped").Scan(&failed); err != nil || !failed {
		t.Fatalf("unresolvable failure not recorded: %v, %v", failed, err)
	}
	if attempts := countEvents(t, rt, `SELECT count(*) FROM events WHERE type='verification.attempted'`); attempts != 0 {
		t.Fatalf("publication attempts saved %d event rows", attempts)
	}
	// A warning saved by an earlier attempt still records the stop without a
	// duplicate warning event.
	if _, err := rt.DB.ExecContext(ctx, `DELETE FROM verification_attempts WHERE run_id=?`, "probe-stopped"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if publisher.updates != 2 || countEvents(t, rt, `SELECT count(*) FROM events WHERE run_id=? AND type='verification.warning'`, "probe-stopped") != 1 {
		t.Fatalf("existing warning duplicated or lost the stop: %d", publisher.updates)
	}
	if err := rt.DB.QueryRow(`SELECT failed FROM verification_attempts WHERE run_id=?`, "probe-stopped").Scan(&failed); err != nil || !failed {
		t.Fatalf("stop marker lost with an existing warning: %v, %v", failed, err)
	}
}

// A transport failure, rate limit or shared-budget timeout stays retryable even
// for a finished run: it does not show the PR can never be updated.
func TestFinishedRunRetriesTransientVerificationFailure(t *testing.T) {
	s, rt, publisher, _ := failingVerificationFlow(t, "github.unavailable")
	ctx := context.Background()
	seedVerificationRun(t, rt, "probe-transient", "STOPPED", 7, 101)
	baseline := countEvents(t, rt, `SELECT count(*) FROM events`)
	growth := []int{}
	for range 3 {
		if _, err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		growth = append(growth, countEvents(t, rt, `SELECT count(*) FROM events`))
	}
	if publisher.updates != 3 {
		t.Fatalf("transient failure was not retried: %d calls", publisher.updates)
	}
	if growth[0] != baseline+1 || growth[1] != growth[0] || growth[2] != growth[0] {
		t.Fatalf("transient failures grew event rows: baseline %d, after reconciliations %v", baseline, growth)
	}
	if warnings := countEvents(t, rt, `SELECT count(*) FROM events WHERE run_id=? AND type='verification.warning'`, "probe-transient"); warnings != 1 {
		t.Fatalf("warnings = %d, want one per source and code", warnings)
	}
	var warning string
	if err := rt.DB.QueryRow(`SELECT json_extract(payload_json,'$.message') FROM events WHERE run_id=? AND type='verification.warning'`, "probe-transient").Scan(&warning); err != nil || !strings.Contains(warning, "will retry") {
		t.Fatalf("transient warning = %q, %v", warning, err)
	}
	var failed bool
	if err := rt.DB.QueryRow(`SELECT failed FROM verification_attempts WHERE run_id=?`, "probe-transient").Scan(&failed); err != nil || failed {
		t.Fatalf("transient failure marked the run stopped: %v, %v", failed, err)
	}
}

// A finished run whose attempt hits the shared five-second budget keeps its
// evidence pending, so a later healthy attempt still publishes it.
func TestFinishedRunPublishesAfterBudgetTimeoutRecovers(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}, prs: map[string]*github.PullRequest{
		"feature": {Number: 101, State: github.Open, Body: "human body"},
	}, head: func(string) string { return "" }}
	_, rt := fixture(t, api)
	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
	if err != nil {
		t.Fatal(err)
	}
	stalled := &stalledEvidenceGitHub{branchGitHub: branchGitHub{api}}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: stalled})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	seedVerificationRun(t, rt, "probe-timeout", "STOPPED", 7, 101)
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if !stalled.bounded {
		t.Fatal("publication was not bounded by the shared budget")
	}
	var failed bool
	if err := rt.DB.QueryRow(`SELECT failed FROM verification_attempts WHERE run_id=?`, "probe-timeout").Scan(&failed); err != nil || failed {
		t.Fatalf("budget timeout marked the run stopped: %v, %v", failed, err)
	}
	healthy := &evidenceGitHub{branchGitHub: branchGitHub{api}}
	s, err = scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: healthy})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if healthy.updates != 1 || !strings.Contains(api.prs["feature"].Body, "Valid evidence") {
		t.Fatalf("timed-out finished-run evidence was not published after recovery: %d", healthy.updates)
	}
}

// A recorded attempt whose call never returned (crash, kill) must not stop a
// finished run: only a returned failure is a delivery result.
func TestInterruptedVerificationAttemptStaysRetryableForFinishedRun(t *testing.T) {
	s, rt, publisher, _ := failingVerificationFlow(t, "github.not_found")
	ctx := context.Background()
	seedVerificationRun(t, rt, "probe-interrupted", "STOPPED", 7, 101)
	if _, err := rt.DB.ExecContext(ctx, `INSERT INTO verification_attempts(run_id,source_attempt,attempts,last_attempt_at,failed)
		VALUES(?,?,1,strftime('%Y-%m-%dT%H:%M:%fZ','now'),0)`, "probe-interrupted", "probe-interrupted-attempt"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if publisher.updates != 1 {
		t.Fatalf("interrupted attempt blocked retry: %d calls", publisher.updates)
	}
	var failed bool
	if err := rt.DB.QueryRow(`SELECT failed FROM verification_attempts WHERE run_id=?`, "probe-interrupted").Scan(&failed); err != nil || !failed {
		t.Fatalf("returned failure not recorded: %v, %v", failed, err)
	}
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if publisher.updates != 1 {
		t.Fatalf("finished run retried after the returned failure: %d calls", publisher.updates)
	}
}

type finiteSlowBodyGitHub struct {
	*fakeGitHub
	calls []int
}

func (f *finiteSlowBodyGitHub) UpdatePullRequest(ctx context.Context, repo string, n int, content github.PullRequestContent) (*github.PullRequest, error) {
	f.calls = append(f.calls, n)
	if n == 101 {
		timer := time.NewTimer(6 * time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil, &fault.Error{Code: "github.unavailable", Message: "Finite slow failure for only first PR"}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.fakeGitHub.UpdatePullRequest(ctx, repo, n, content)
}
func TestSlowVerificationPublicationDoesNotStarveHealthyPRAfterRestart(t *testing.T) {
	// STOPPED runs keep retrying transient failures, so durable
	// least-recently-attempted rotation is what lets the healthy PR through
	// after the restart.
	api := &fakeGitHub{issues: map[string][]github.Issue{}, prs: map[string]*github.PullRequest{
		"first": {Number: 101, Body: "human first"}, "second": {Number: 102, Body: "human second"},
	}}
	_, rt := fixture(t, api)
	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.PRComments = false
	client := &finiteSlowBodyGitHub{fakeGitHub: api}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: client})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i, id := range []string{"probe-first", "probe-second"} {
		seedVerificationRun(t, rt, id, "STOPPED", i+1, 101+i)
	}
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	root := rt.Workspace.Root
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := app.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Close() })
	s, err = scheduler.New(cfg, schedulerResources(restarted), scheduler.Dependencies{GitHub: client})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("body publication call order: %v", client.calls)
	if len(client.calls) != 3 || client.calls[0] != 101 || client.calls[1] != 102 || client.calls[2] != 101 {
		t.Fatalf("pending evidence did not rotate after restart: %v", client.calls)
	}
	delivered := false
	for _, n := range client.calls {
		if n == 102 {
			delivered = true
		}
	}
	if !delivered {
		t.Fatal("healthy second PR was never attempted after repeated bounded retries of finite-slow first PR")
	}
	if !strings.Contains(api.prs["second"].Body, "Valid evidence") {
		t.Fatal("healthy second PR evidence was not published")
	}
}
