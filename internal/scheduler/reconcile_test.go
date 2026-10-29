package scheduler_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestOrphanedClaimIsReportedWithoutDispatchOrLabelChanges(t *testing.T) {
	t.Parallel()

	issue := testkit.Ready(7)
	issue.Labels = append(issue.Labels, github.Label{Name: "agent-running"})
	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {issue}}}
	s, runtime := testkit.Fixture(t, api)
	api.Mutate = func(string, string, int, string) error { t.Fatal("orphan labels were changed"); return nil }
	for range 2 {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := s.Runs(context.Background())
	if err != nil || len(runs) != 0 {
		t.Fatalf("orphan dispatched: %v, %v", runs, err)
	}
	history, err := runtime.Events.History(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Type != "reconcile.orphaned_claim" {
		t.Fatalf("orphan report = %+v", history)
	}
	var finding scheduler.Finding
	if err := json.Unmarshal(history[0].Payload, &finding); err != nil {
		t.Fatal(err)
	}
	if finding.Repository != "owner/repo" || finding.IssueNumber != 7 {
		t.Fatalf("wrong orphan identity: %+v", finding)
	}
}

func TestRunWithMissingContextKeepsItsOrphanedClaim(t *testing.T) {
	t.Parallel()

	issue := testkit.Ready(7)
	issue.Labels = []github.Label{{Name: "agent-running"}, {Name: "ready-for-agent"}}
	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {issue}}}
	s, runtime := testkit.Fixture(t, api)
	ctx := context.Background()
	for _, request := range []workflow.Request{{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7}, {Trigger: workflow.ClaimSucceeded}, {Trigger: workflow.WorktreeReady}} {
		if _, err := runtime.Workflow.Transition(ctx, "lost-context", request); err != nil {
			t.Fatal(err)
		}
	}
	api.Mutate = func(string, string, int, string) error { t.Fatal("unrecoverable claim labels changed"); return nil }
	for range 2 {
		if err := s.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := s.Runs(ctx)
	if err != nil || len(runs) != 1 || runs[0].State != workflow.NeedsAttention || runs[0].LastErrorCode != "reconcile.context_missing" {
		t.Fatalf("unrecoverable run was resumed: %+v, %v", runs, err)
	}
	history, err := runtime.Events.History(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range history {
		if event.Type == "reconcile.orphaned_claim" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unrecoverable claim not reported: %+v", history)
	}
}

func TestGitHubFailureClassification(t *testing.T) {
	t.Parallel()

	unavailable := &fault.Error{Code: "github.unavailable", Message: "network is unreachable"}
	rateLimited := &fault.Error{Code: "github.rate_limited", Message: "API rate limit exceeded"}
	ownership := &fault.Error{Code: "git.ownership_missing", Message: "Run has no persisted Git ownership"}
	store := &fault.Error{Code: "internal.run_store", Message: "Could not persist or read the run", Err: sql.ErrConnDone}
	tmux := &fault.Error{Code: "phase.tmux_failed", Message: "tmux failed", Err: errors.New("exit status 1")}
	for _, tc := range []struct {
		name string
		Err  error
		want bool
	}{
		{"none", nil, false},
		{"unreachable GitHub", unavailable, true},
		{"rate limited GitHub", rateLimited, true},
		{"cancelled GitHub", &fault.Error{Code: "github.canceled", Err: context.Canceled}, true},
		{"timed out GitHub", &fault.Error{Code: "github.timeout", Err: context.DeadlineExceeded}, true},
		{"unanswered command", &fault.Error{Code: "github.command_failed", Message: "unrecognized failure"}, true},
		{"unclean response", &fault.Error{Code: "github.invalid_response", Message: "expected an issue array"}, true},
		{"joined GitHub failures", errors.Join(unavailable, rateLimited), true},
		{"wrapped GitHub failure", fmt.Errorf("reconcile existing runs: %w", unavailable), true},
		{"GitHub failure wrapping a transport error", &fault.Error{Code: "github.unavailable", Err: errors.New("dial tcp: connection refused")}, true},
		{"coded wrapper classifying its cause", &fault.Error{Code: "internal.scheduler", Message: "operation failed", Err: unavailable}, true},
		{"pending handback around a GitHub failure", &fault.Error{Code: "handback.publication_pending", Message: "Publication or reconciliation is incomplete", Err: unavailable}, true},
		{"pending handback around a local failure", &fault.Error{Code: "handback.publication_pending", Message: "Publication or reconciliation is incomplete", Err: tmux}, false},
		{"conclusive forbidden", &fault.Error{Code: "github.forbidden", Message: "Resource not accessible by token"}, false},
		{"conclusive not found", &fault.Error{Code: "github.not_found", Message: "Not Found"}, false},
		{"conclusive login", &fault.Error{Code: "github.not_logged_in", Message: "run gh auth login"}, false},
		{"conclusive input", &fault.Error{Code: "github.invalid_input", Message: "expected owner/repo"}, false},
		{"scheduler-minted GitHub code", &fault.Error{Code: "github.issue_closed", Message: "Issue is no longer open"}, false},
		{"Git ownership", ownership, false},
		{"local store", store, false},
		{"raw storage error", sql.ErrConnDone, false},
		{"runner failure", tmux, false},
		{"GitHub failure joined with local state", errors.Join(unavailable, ownership), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := scheduler.GitHubFailure(tc.Err); got != tc.want {
				t.Fatalf("GitHubFailure(%v) = %v, want %v", tc.Err, got, tc.want)
			}
		})
	}
}

func TestPendingHandbackDuringOutageDefersInsteadOfRefusingStartup(t *testing.T) {
	t.Parallel()

	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {}}}
	s, runtime := testkit.Configured(t, testkit.UnavailablePullRequests{FakeGitHub: api}, "repositories:\n  - repo: owner/repo\n", testkit.HandbackGit{})
	ctx := context.Background()
	run, err := runtime.Workflow.Transition(ctx, "pending-handback", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(runtime.Workspace.Root, "runs", run.ID), 0700); err != nil {
		t.Fatal(err)
	}
	issue := `{"number":7,"title":"Implement this","state":"open","created_at":"2026-01-01T00:00:00Z","labels":[{"name":"ready-for-agent"}]}`
	if _, err := runtime.DB.ExecContext(ctx, "INSERT INTO scheduler_runs(run_id,issue_json) VALUES(?,?)", run.ID, issue); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.DB.ExecContext(ctx, "INSERT INTO run_handbacks(run_id,pending,snapshot_json) VALUES(?,1,?)", run.ID, `{"pending":true,"requested_at":"2026-01-01T00:00:00Z"}`); err != nil {
		t.Fatal(err)
	}
	_, err = s.Reconcile(ctx)
	var coded *fault.Error
	if !errors.As(err, &coded) || coded.Code != "handback.publication_pending" {
		t.Fatalf("reconcile error = %v, want a pending handback publication", err)
	}
	if !scheduler.GitHubFailure(err) {
		t.Fatalf("pending handback during an outage must defer startup: %v", err)
	}
}

func TestDeferredGitHubFailureStillGatesClaimsUntilReconcileSucceeds(t *testing.T) {
	t.Parallel()

	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}}
	gated := &testkit.ToggledGitHub{FakeGitHub: api, Fail: true}
	s, _ := testkit.Configured(t, gated, "repositories:\n  - repo: owner/repo\n", testkit.RejectingGit{})
	ctx := context.Background()
	for range 2 {
		if _, err := s.Reconcile(ctx); !scheduler.GitHubFailure(err) {
			t.Fatalf("startup reconcile error = %v, want a deferred GitHub failure", err)
		}
		if err := s.Tick(ctx); !scheduler.GitHubFailure(err) {
			t.Fatalf("tick with GitHub unavailable = %v, want a deferred GitHub failure", err)
		}
		runs, err := s.Runs(ctx)
		if err != nil || len(runs) != 0 {
			t.Fatalf("claimed before a successful reconcile: %+v, %v", runs, err)
		}
	}
	gated.Fail = false
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(ctx)
	if err != nil || len(runs) != 1 || runs[0].State != workflow.NeedsAttention {
		t.Fatalf("recovered reconcile did not resume dispatch: %+v, %v", runs, err)
	}
}

func TestEnabledRepositoryGitHubFailureDoesNotBlockOtherRepositories(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{
		"owner/broken": {testkit.Ready(1)},
		"owner/ok":     {testkit.Ready(2)},
	}}
	failing := &testkit.RepositoryFailureGitHub{FakeGitHub: api, Failures: map[string]error{
		"owner/broken": &fault.Error{Code: "github.not_found", Message: "repository owner/broken was renamed or deleted"},
	}}
	runtime, err := app.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close() })
	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/broken\n  - repo: owner/ok\n"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: failing, Git: testkit.RejectingGit{}})
	if err != nil {
		t.Fatal(err)
	}

	err = s.Tick(ctx)
	if !scheduler.GitHubFailure(err) || !strings.Contains(err.Error(), "github.not_found") {
		t.Fatalf("tick error = %v, want the broken repository's scoped failure", err)
	}
	runs, err := s.Runs(ctx)
	if err != nil || len(runs) != 1 || !strings.EqualFold(runs[0].Repository, "owner/ok") {
		t.Fatalf("claims blocked by another repository's GitHub failure: %+v, %v", runs, err)
	}
	failures := s.ReconcileFailures()
	if len(failures) != 1 || failures[0].Code != "reconcile.repository_failed" || !strings.EqualFold(failures[0].Repository, "owner/broken") {
		t.Fatalf("scoped failures = %+v", failures)
	}
	report, err := s.Reconcile(ctx)
	if !scheduler.GitHubFailure(err) {
		t.Fatalf("reconcile error = %v, want a deferred GitHub failure", err)
	}
	found := false
	for _, finding := range report.Findings {
		if finding.Code == "reconcile.repository_failed" && strings.EqualFold(finding.Repository, "owner/broken") {
			found = true
		}
	}
	if !found {
		t.Fatalf("reconcile report hid the failing repository: %+v", report.Findings)
	}

	server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "owner/broken") || !strings.Contains(recorder.Body.String(), "reconcile.repository_failed") {
		t.Fatalf("dashboard hid the failing repository: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestQueueReportKeepsReadableRepositoriesWhenOneFails(t *testing.T) {
	t.Parallel()

	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{
		"owner/broken": {testkit.Ready(1)},
		"owner/ok":     {testkit.Ready(2)},
	}}
	failing := &testkit.RepositoryFailureGitHub{FakeGitHub: api, Failures: map[string]error{
		"owner/broken": &fault.Error{Code: "github.forbidden", Message: "Resource not accessible by integration"},
	}}
	s, _ := testkit.Configured(t, failing, "repositories:\n  - repo: owner/broken\n  - repo: owner/ok\n", testkit.RejectingGit{})

	report, err := s.QueueReport(context.Background())
	if !scheduler.GitHubFailure(err) {
		t.Fatalf("queue error = %v, want the broken repository's scoped failure", err)
	}
	if len(report.Issues) != 1 || !strings.EqualFold(report.Issues[0].Repository, "owner/ok") {
		t.Fatalf("readable repository disappeared from the queue: %+v", report.Issues)
	}
	if len(report.Failures) != 1 || !strings.EqualFold(report.Failures[0].Repository, "owner/broken") || report.Failures[0].Code != "reconcile.repository_failed" {
		t.Fatalf("queue failures = %+v", report.Failures)
	}
}

func TestScopedGitHubFindingsDoNotPersistRawDiagnostics(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	secret := "ghp_example_secret_value"
	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{
		"owner/broken": {testkit.Ready(1)},
		"owner/ok":     {testkit.Ready(2)},
	}}
	failing := &testkit.RepositoryFailureGitHub{FakeGitHub: api, Failures: map[string]error{
		"owner/broken": &fault.Error{Code: "github.unavailable", Message: "authorization: token " + secret, Err: errors.New("stderr: token " + secret)},
	}}
	s, runtime := testkit.Configured(t, failing, "repositories:\n  - repo: owner/broken\n  - repo: owner/ok\n", testkit.RejectingGit{})

	report, err := s.Reconcile(ctx)
	if !scheduler.GitHubFailure(err) {
		t.Fatalf("reconcile error = %v, want a scoped GitHub failure", err)
	}
	if !strings.Contains(err.Error(), secret) {
		t.Fatalf("reconcile error discarded the raw diagnostic: %v", err)
	}
	for _, finding := range append(report.Findings, s.ReconcileFailures()...) {
		if strings.Contains(finding.Message, secret) {
			t.Fatalf("finding persisted raw diagnostics: %+v", finding)
		}
		if !strings.Contains(finding.Message, "github.unavailable") || !strings.Contains(finding.Message, "GitHub is unreachable") {
			t.Fatalf("finding lacks its safe code and explanation: %+v", finding)
		}
	}
	history, err := runtime.Events.History(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range history {
		if strings.Contains(string(event.Payload), secret) {
			t.Fatalf("durable event persisted raw diagnostics: %s", event.Payload)
		}
	}
}

func TestDisabledRepositoryGitHubFailureDoesNotBlockDispatch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/ok": {testkit.Ready(2)}}}
	failing := &testkit.RepositoryFailureGitHub{FakeGitHub: api, Failures: map[string]error{
		"owner/broken": &fault.Error{Code: "github.not_found", Message: "repository owner/broken was renamed or deleted"},
	}}
	s, _ := testkit.Configured(t, failing, "repositories:\n  - repo: owner/broken\n    enabled: false\n  - repo: owner/ok\n", testkit.RejectingGit{})

	if err := s.Tick(ctx); !scheduler.GitHubFailure(err) {
		t.Fatalf("tick error = %v, want a deferred GitHub failure", err)
	}
	runs, err := s.Runs(ctx)
	if err != nil || len(runs) != 1 || !strings.EqualFold(runs[0].Repository, "owner/ok") {
		t.Fatalf("disabled repository blocked dispatch: %+v, %v", runs, err)
	}
}

func TestRunGitHubFailureDoesNotBlockOtherRepositories(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{
		"owner/stuck": {},
		"owner/ok":    {testkit.Ready(2)},
	}}
	failing := &testkit.FailingPullRequests{FakeGitHub: api, Err: &fault.Error{Code: "github.forbidden", Message: "Resource not accessible by integration"}}
	s, runtime := testkit.Configured(t, failing, "concurrency: 2\nrepositories:\n  - repo: owner/stuck\n  - repo: owner/ok\n", testkit.RejectingGit{})

	run, err := runtime.Workflow.Transition(ctx, "stuck-run", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/stuck", IssueNumber: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, trigger := range []workflow.Trigger{workflow.ClaimSucceeded, workflow.WorktreeReady} {
		if run, err = runtime.Workflow.Transition(ctx, run.ID, workflow.Request{Trigger: trigger}); err != nil {
			t.Fatal(err)
		}
	}
	number, round := 9, 1
	if _, err := runtime.Workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.ImplementSucceeded, Metadata: workflow.MetadataPatch{PRNumber: &number, ReviewRound: &round}}); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if err := s.Tick(ctx); !scheduler.GitHubFailure(err) {
			t.Fatalf("tick error = %v, want the stuck run's scoped failure", err)
		}
	}
	runs, err := s.Runs(ctx)
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	claimed := false
	for _, run := range runs {
		if run.ID != "stuck-run" && strings.EqualFold(run.Repository, "owner/ok") {
			claimed = true
		}
	}
	if !claimed {
		t.Fatalf("stuck run blocked another repository's claim: %+v", runs)
	}
	failures := s.ReconcileFailures()
	if len(failures) != 1 || failures[0].Code != "reconcile.run_failed" || failures[0].RunID != "stuck-run" || !strings.EqualFold(failures[0].Repository, "owner/stuck") {
		t.Fatalf("scoped failures = %+v", failures)
	}
}
