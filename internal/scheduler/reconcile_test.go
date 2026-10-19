package scheduler_test

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestOrphanedClaimIsReportedWithoutDispatchOrLabelChanges(t *testing.T) {
	issue := ready(7)
	issue.Labels = append(issue.Labels, github.Label{Name: "agent-running"})
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {issue}}}
	s, runtime := fixture(t, api)
	api.mutate = func(string, string, int, string) error { t.Fatal("orphan labels were changed"); return nil }
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

func TestOrphanedWorktreeAndSessionAreReportedAndPreservedWhilePaused(t *testing.T) {
	s, runtime, _, _, _, r := localFlow(t, successfulScript)
	ctx := context.Background()
	gitRun, err := managedgit.New(runtime.Workspace).Prepare(ctx, managedgit.PrepareRequest{Repository: "owner/repo", RunID: "orphan", IssueNumber: 42})
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(gitRun.Path, "keep.txt")
	if err := os.WriteFile(marker, []byte("preserve me"), 0600); err != nil {
		t.Fatal(err)
	}
	ref, err := r.StartSession(ctx, runner.SessionRequest{RunID: "orphan", Phase: "implement", Attempt: 1, PhaseDir: filepath.Join(runtime.Workspace.Root, "runs", "orphan", "phases", "implement-0-1"), Command: runner.ExecRequest{Executable: "/bin/sleep", Args: []string{"30"}, Dir: gitRun.Path}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Pause(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	history, err := runtime.Events.History(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	foundTree, foundSession := false, false
	for _, event := range history {
		var finding scheduler.Finding
		if err := json.Unmarshal(event.Payload, &finding); err != nil {
			t.Fatal(err)
		}
		if event.Type == "reconcile.orphaned_worktree" {
			actual, err := os.Stat(finding.Path)
			if err != nil {
				t.Fatal(err)
			}
			expected, err := os.Stat(gitRun.Path)
			if err != nil || !os.SameFile(actual, expected) {
				t.Fatalf("wrong orphan worktree: %+v, %v", finding, err)
			}
			foundTree = true
		}
		if event.Type == "reconcile.orphaned_session" && finding.Session == ref.Name {
			foundSession = true
		}
	}
	if !foundTree || !foundSession {
		t.Fatalf("missing orphan report: %+v", history)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "preserve me" {
		t.Fatalf("orphan files changed: %q, %v", data, err)
	}
	status, err := r.SessionStatus(ctx, ref)
	if err != nil || status.State != runner.SessionRunning {
		t.Fatalf("orphan session stopped: %+v, %v", status, err)
	}
	runs, err := s.Runs(ctx)
	if err != nil || len(runs) != 0 {
		t.Fatalf("reconciliation dispatched new work: %+v, %v", runs, err)
	}
}

func TestRunWithMissingContextKeepsItsOrphanedClaim(t *testing.T) {
	issue := ready(7)
	issue.Labels = []github.Label{{Name: "agent-running"}, {Name: "ready-for-agent"}}
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {issue}}}
	s, runtime := fixture(t, api)
	ctx := context.Background()
	for _, request := range []workflow.Request{{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7}, {Trigger: workflow.ClaimSucceeded}, {Trigger: workflow.WorktreeReady}} {
		if _, err := runtime.Workflow.Transition(ctx, "lost-context", request); err != nil {
			t.Fatal(err)
		}
	}
	api.mutate = func(string, string, int, string) error { t.Fatal("unrecoverable claim labels changed"); return nil }
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

func TestMissingOwnedWorktreeRequiresAttentionWithoutReportingAnOrphan(t *testing.T) {
	s, runtime, _, _, _, _ := localFlow(t, `while [ ! -f release ]; do /bin/sleep 0.02; done`)
	ctx := context.Background()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	paths, _ := filepath.Glob(filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", "*"))
	if len(paths) != 1 {
		t.Fatalf("initial worktrees: %v", paths)
	}
	if err := os.RemoveAll(paths[0]); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(ctx)
	if err != nil || len(runs) != 1 || runs[0].State != workflow.NeedsAttention {
		t.Fatalf("missing worktree was resumed: %+v, %v", runs, err)
	}
	history, err := runtime.Events.History(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range history {
		if event.Type == "reconcile.orphaned_worktree" {
			t.Fatalf("missing owned worktree was reported as orphaned: %s", event.Payload)
		}
	}
}

type controlPlaneInput struct {
	Config    config.Config
	Workspace string
	Socket    string
}

// The parent kills this real control-plane process after its first dispatch.
// Agent and wrapper processes live on a separate tmux server and survive it.
func TestReconciliationControlPlaneProcess(t *testing.T) {
	path := os.Getenv("MERGEYARD_TEST_CONTROL_PLANE_INPUT")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var input controlPlaneInput
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	runtime, err := app.Open(context.Background(), input.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}}
	r := runner.NewLocal(runner.Options{SocketName: input.Socket})
	s, err := scheduler.New(input.Config, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(context.Background())
	if err != nil || len(runs) != 1 || runs[0].State != workflow.Active || runs[0].Phase != workflow.Implement {
		t.Fatalf("initial dispatch: %+v, %v", runs, err)
	}
	fmt.Println("control-plane-ready")
	time.Sleep(time.Minute)
}

func TestRestartAfterControlPlaneKillPreservesImplementAttempt(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		for _, finishedOffline := range []bool{false, true} {
			name := "agent-still-running"
			if finishedOffline {
				name = "agent-finished-offline"
			}
			t.Run(agent+"/"+name, func(t *testing.T) {
				_, initial, api, remote, cfg, _ := localFlow(t, `while [ ! -f release ]; do /bin/sleep 0.02; done
`+successfulScript)
				if agent == "codex" {
					executable := filepath.Join(t.TempDir(), "fake-codex")
					script := "#!/bin/sh\n" + codexIdentity + "\nwhile [ ! -f release ]; do /bin/sleep 0.02; done\n" + codexImplementation + "\n"
					if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
						t.Fatal(err)
					}
					cfg.Agents.Codex.Executable = executable
					cfg.Repositories[0].Implementer.Agent = "codex"
				}
				root := initial.Workspace.Root
				if err := initial.Close(); err != nil {
					t.Fatal(err)
				}
				socket := fmt.Sprintf("mergeyard-reconciliation-test-%d", time.Now().UnixNano())
				t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
				input, err := json.Marshal(controlPlaneInput{Config: cfg, Workspace: root, Socket: socket})
				if err != nil {
					t.Fatal(err)
				}
				inputPath := filepath.Join(t.TempDir(), "control-plane.json")
				if err := os.WriteFile(inputPath, input, 0600); err != nil {
					t.Fatal(err)
				}
				executable, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, executable, "-test.run=^TestReconciliationControlPlaneProcess$")
				cmd.Env = append(os.Environ(), "MERGEYARD_TEST_CONTROL_PLANE_INPUT="+inputPath)
				cmd.Stderr = os.Stderr
				stdout, err := cmd.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
				scanner := bufio.NewScanner(stdout)
				if !scanner.Scan() || scanner.Text() != "control-plane-ready" {
					t.Fatalf("control plane never became ready: %q, %v", scanner.Text(), scanner.Err())
				}
				if err := cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				cmd.Wait()
				// GitHub claim labels outlive the killed process just as they do in production.
				api.issues["owner/repo"][0].Labels = []github.Label{{Name: "agent-running"}}
				trees, _ := filepath.Glob(filepath.Join(root, "worktrees", "owner-repo", "*"))
				if len(trees) != 1 {
					t.Fatalf("initial worktree count: %v", trees)
				}
				r := runner.NewLocal(runner.Options{SocketName: socket})
				var runtime *app.Runtime
				if finishedOffline {
					if err := os.WriteFile(filepath.Join(trees[0], "release"), nil, 0600); err != nil {
						t.Fatal(err)
					}
					deadline := time.Now().Add(5 * time.Second)
					for {
						exits, _ := filepath.Glob(filepath.Join(root, "runs", "*", "phases", "*", "exit.json"))
						if len(exits) == 1 {
							break
						}
						if time.Now().After(deadline) {
							t.Fatal("agent did not exit while offline")
						}
						time.Sleep(10 * time.Millisecond)
					}
				}
				runtime, err = app.Open(context.Background(), root)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { runtime.Close() })
				s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
				if err != nil {
					t.Fatal(err)
				}
				if !finishedOffline {
					for range 2 {
						if err := s.Tick(context.Background()); err != nil {
							t.Fatal(err)
						}
					}
					runs, err := s.Runs(context.Background())
					if err != nil || len(runs) != 1 || runs[0].State != workflow.Active || runs[0].Phase != workflow.Implement {
						t.Fatalf("live agent was not preserved: %+v, %v", runs, err)
					}
					if err := os.WriteFile(filepath.Join(trees[0], "release"), nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
				run := finish(t, s, workflow.Active, workflow.Review)
				for range 2 {
					if err := s.Tick(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				runs, err := s.Runs(context.Background())
				phases, _ := filepath.Glob(filepath.Join(root, "runs", run.ID, "phases", "*"))
				if err != nil || len(runs) != 1 || len(phases) != 2 || api.creations != 1 {
					t.Fatalf("duplicate recovery work: runs=%v, phases=%v, PRs=%d, err=%v", runs, phases, api.creations, err)
				}
				if got := gitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7"); got != "1" {
					t.Fatalf("recovery commits = %s", got)
				}
				data, err := os.ReadFile(filepath.Join(phases[0], "result.json"))
				if err != nil {
					t.Fatal(err)
				}
				var result struct {
					Status string `json:"status"`
				}
				if err := json.Unmarshal(data, &result); err != nil || result.Status != "success" {
					t.Fatalf("recovered result: %s, %v", data, err)
				}
			})
		}
	}
}

func TestGitHubFailureClassification(t *testing.T) {
	unavailable := &fault.Error{Code: "github.unavailable", Message: "network is unreachable"}
	rateLimited := &fault.Error{Code: "github.rate_limited", Message: "API rate limit exceeded"}
	ownership := &fault.Error{Code: "git.ownership_missing", Message: "Run has no persisted Git ownership"}
	store := &fault.Error{Code: "internal.run_store", Message: "Could not persist or read the run", Err: sql.ErrConnDone}
	tmux := &fault.Error{Code: "phase.tmux_failed", Message: "tmux failed", Err: errors.New("exit status 1")}
	for _, tc := range []struct {
		name string
		err  error
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
			if got := scheduler.GitHubFailure(tc.err); got != tc.want {
				t.Fatalf("GitHubFailure(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// handbackGit satisfies HandbackGit; the PR read fails before publication, so
// every publication call is a bug.
type handbackGit struct{ rejectingGit }

func (handbackGit) InspectHandback(context.Context, managedgit.Run, string) (string, error) {
	panic("handback inspection must not run when the PR read fails")
}
func (handbackGit) CommitHandback(context.Context, managedgit.Run, string) (managedgit.CommitResult, error) {
	panic("handback commit must not run when the PR read fails")
}
func (handbackGit) PushHandback(context.Context, managedgit.Run, string, string) error {
	panic("handback push must not run when the PR read fails")
}

type unavailablePullRequests struct{ *fakeGitHub }

func (unavailablePullRequests) GetPullRequest(context.Context, string, int) (*github.PullRequest, error) {
	return nil, &fault.Error{Code: "github.unavailable", Message: "dial tcp: connection refused"}
}
func (unavailablePullRequests) FindPullRequest(context.Context, string, string) (*github.PullRequest, error) {
	return nil, &fault.Error{Code: "github.unavailable", Message: "dial tcp: connection refused"}
}

func TestPendingHandbackDuringOutageDefersInsteadOfRefusingStartup(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {}}}
	s, runtime := configured(t, unavailablePullRequests{fakeGitHub: api}, "repositories:\n  - repo: owner/repo\n", handbackGit{})
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

type toggledGitHub struct {
	*fakeGitHub
	fail bool
}

func (g *toggledGitHub) ListLabeledIssues(ctx context.Context, repo string, labels []string) ([]github.Issue, error) {
	if g.fail {
		return nil, &fault.Error{Code: "github.unavailable", Message: "dial tcp: connection refused"}
	}
	return g.fakeGitHub.ListLabeledIssues(ctx, repo, labels)
}

func TestDeferredGitHubFailureStillGatesClaimsUntilReconcileSucceeds(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}}
	gated := &toggledGitHub{fakeGitHub: api, fail: true}
	s, _ := configured(t, gated, "repositories:\n  - repo: owner/repo\n", rejectingGit{})
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
	gated.fail = false
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(ctx)
	if err != nil || len(runs) != 1 || runs[0].State != workflow.NeedsAttention {
		t.Fatalf("recovered reconcile did not resume dispatch: %+v, %v", runs, err)
	}
}

// repositoryFailureGitHub fails listings for selected repositories and leaves
// every other boundary call to the normal fake.
type repositoryFailureGitHub struct {
	*fakeGitHub
	failures map[string]error
}

func (g *repositoryFailureGitHub) ListLabeledIssues(ctx context.Context, repo string, labels []string) ([]github.Issue, error) {
	if err := g.failures[repo]; err != nil {
		return nil, err
	}
	return g.fakeGitHub.ListLabeledIssues(ctx, repo, labels)
}

func TestEnabledRepositoryGitHubFailureDoesNotBlockOtherRepositories(t *testing.T) {
	ctx := context.Background()
	api := &fakeGitHub{issues: map[string][]github.Issue{
		"owner/broken": {ready(1)},
		"owner/ok":     {ready(2)},
	}}
	failing := &repositoryFailureGitHub{fakeGitHub: api, failures: map[string]error{
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
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: failing, Git: rejectingGit{}})
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
	server.ServeHTTP(recorder, dashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "owner/broken") || !strings.Contains(recorder.Body.String(), "reconcile.repository_failed") {
		t.Fatalf("dashboard hid the failing repository: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestQueueReportKeepsReadableRepositoriesWhenOneFails(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{
		"owner/broken": {ready(1)},
		"owner/ok":     {ready(2)},
	}}
	failing := &repositoryFailureGitHub{fakeGitHub: api, failures: map[string]error{
		"owner/broken": &fault.Error{Code: "github.forbidden", Message: "Resource not accessible by integration"},
	}}
	s, _ := configured(t, failing, "repositories:\n  - repo: owner/broken\n  - repo: owner/ok\n", rejectingGit{})

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
	ctx := context.Background()
	secret := "ghp_example_secret_value"
	api := &fakeGitHub{issues: map[string][]github.Issue{
		"owner/broken": {ready(1)},
		"owner/ok":     {ready(2)},
	}}
	failing := &repositoryFailureGitHub{fakeGitHub: api, failures: map[string]error{
		"owner/broken": &fault.Error{Code: "github.unavailable", Message: "authorization: token " + secret, Err: errors.New("stderr: token " + secret)},
	}}
	s, runtime := configured(t, failing, "repositories:\n  - repo: owner/broken\n  - repo: owner/ok\n", rejectingGit{})

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
	ctx := context.Background()
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/ok": {ready(2)}}}
	failing := &repositoryFailureGitHub{fakeGitHub: api, failures: map[string]error{
		"owner/broken": &fault.Error{Code: "github.not_found", Message: "repository owner/broken was renamed or deleted"},
	}}
	s, _ := configured(t, failing, "repositories:\n  - repo: owner/broken\n    enabled: false\n  - repo: owner/ok\n", rejectingGit{})

	if err := s.Tick(ctx); !scheduler.GitHubFailure(err) {
		t.Fatalf("tick error = %v, want a deferred GitHub failure", err)
	}
	runs, err := s.Runs(ctx)
	if err != nil || len(runs) != 1 || !strings.EqualFold(runs[0].Repository, "owner/ok") {
		t.Fatalf("disabled repository blocked dispatch: %+v, %v", runs, err)
	}
}

// failingPullRequests keeps the persisted run alive while its PR always
// returns the same conclusive GitHub error.
type failingPullRequests struct {
	*fakeGitHub
	err error
}

func (g *failingPullRequests) GetPullRequest(context.Context, string, int) (*github.PullRequest, error) {
	return nil, g.err
}

func TestRunGitHubFailureDoesNotBlockOtherRepositories(t *testing.T) {
	ctx := context.Background()
	api := &fakeGitHub{issues: map[string][]github.Issue{
		"owner/stuck": {},
		"owner/ok":    {ready(2)},
	}}
	failing := &failingPullRequests{fakeGitHub: api, err: &fault.Error{Code: "github.forbidden", Message: "Resource not accessible by integration"}}
	s, runtime := configured(t, failing, "concurrency: 2\nrepositories:\n  - repo: owner/stuck\n  - repo: owner/ok\n", rejectingGit{})

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
