package scheduler_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type fakeGitHub struct {
	issues    map[string][]github.Issue
	blockers  map[int][]github.Issue
	prs       map[string]*github.PullRequest
	creations int
	mutate    func(action, repo string, n int, label string) error
}

func (f *fakeGitHub) ListOpenIssues(_ context.Context, repo string) ([]github.Issue, error) {
	return f.issues[repo], nil
}
func (f *fakeGitHub) GetIssue(_ context.Context, repo string, n int) (github.Issue, error) {
	for _, issue := range f.issues[repo] {
		if issue.Number == n {
			return issue, nil
		}
	}
	return github.Issue{}, &fault.Error{Code: "github.not_found", Message: "Issue not found"}
}
func (f *fakeGitHub) GetPullRequest(_ context.Context, _ string, n int) (*github.PullRequest, error) {
	for _, pr := range f.prs {
		if pr.Number == n {
			return pr, nil
		}
	}
	return nil, &fault.Error{Code: "github.not_found", Message: "PR not found"}
}
func (f *fakeGitHub) UnresolvedBlockers(_ context.Context, _ string, n int) ([]github.Issue, error) {
	return f.blockers[n], nil
}
func (f *fakeGitHub) AddLabel(_ context.Context, repo string, n int, label string) error {
	if f.mutate != nil {
		if err := f.mutate("add", repo, n, label); err != nil {
			return err
		}
	}
	for i := range f.issues[repo] {
		if f.issues[repo][i].Number == n {
			if !has(f.issues[repo][i], label) {
				f.issues[repo][i].Labels = append(f.issues[repo][i].Labels, github.Label{Name: label})
			}
		}
	}
	return nil
}
func (f *fakeGitHub) RemoveLabel(_ context.Context, repo string, n int, label string) error {
	if f.mutate != nil {
		if err := f.mutate("remove", repo, n, label); err != nil {
			return err
		}
	}
	for i := range f.issues[repo] {
		if f.issues[repo][i].Number == n {
			var labels []github.Label
			for _, l := range f.issues[repo][i].Labels {
				if l.Name != label {
					labels = append(labels, l)
				}
			}
			f.issues[repo][i].Labels = labels
		}
	}
	return nil
}
func (f *fakeGitHub) FindOpenPullRequest(_ context.Context, _ string, branch string) (*github.PullRequest, error) {
	return f.prs[branch], nil
}
func (f *fakeGitHub) CreateDraftPullRequest(_ context.Context, repo, branch, base string, content github.PullRequestContent) (*github.PullRequest, error) {
	f.creations++
	if f.prs == nil {
		f.prs = map[string]*github.PullRequest{}
	}
	pr := &github.PullRequest{Number: 100 + f.creations, URL: "https://github.com/" + repo + "/pull/101", Draft: true, State: github.Open}
	pr.Head.Ref = branch
	pr.Head.Repo.FullName = repo
	body, err := github.GeneratePullRequestBody(content)
	if err != nil {
		return nil, err
	}
	pr.Body = body
	f.prs[branch] = pr
	return pr, nil
}
func ready(n int) github.Issue {
	return github.Issue{Number: n, Title: "Implement this", State: github.Open, CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Labels: []github.Label{{Name: "ready-for-agent"}}}
}
func fixture(t *testing.T, api *fakeGitHub) (*scheduler.Scheduler, *app.Runtime) {
	t.Helper()
	runtime, err := app.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close() })
	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api})
	if err != nil {
		t.Fatal(err)
	}
	return s, runtime
}
func TestBlockedAndUnreadyIssuesAreNeverClaimed(t *testing.T) {
	unready := ready(2)
	unready.Labels = nil
	running := ready(3)
	running.Labels = append(running.Labels, github.Label{Name: "agent-running"})
	attention := ready(4)
	attention.Labels = append(attention.Labels, github.Label{Name: "agent-needs-attention"})
	closed := ready(5)
	closed.State = github.Closed
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(1), unready, running, attention, closed}}, blockers: map[int][]github.Issue{1: {ready(99)}}}
	s, _ := fixture(t, api)
	for range 3 {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := s.Runs(context.Background())
	if err != nil || len(runs) != 0 {
		t.Fatalf("ineligible issues created runs: %+v, %v", runs, err)
	}
}

func gitCommand(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s: %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}
func localFlow(t *testing.T, script string) (*scheduler.Scheduler, *app.Runtime, *fakeGitHub, string, config.Config, runner.Runner) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux required")
	}
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	gitCommand(t, root, "init", "--bare", "--initial-branch=main", remote)
	gitCommand(t, root, "init", "--initial-branch=main", seed)
	configPath := filepath.Join(root, "gitconfig")
	gitCommand(t, root, "config", "--file", configPath, "user.name", "Mergeyard Test")
	gitCommand(t, root, "config", "--file", configPath, "user.email", "mergeyard-test@example.com")
	gitCommand(t, root, "config", "--file", configPath, "url."+remote+".insteadOf", "https://github.com/owner/repo.git")
	t.Setenv("GIT_CONFIG_GLOBAL", configPath)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("initial\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, seed, "add", ".")
	gitCommand(t, seed, "commit", "-m", "initial")
	gitCommand(t, seed, "remote", "add", "origin", remote)
	gitCommand(t, seed, "push", "origin", "main")
	executable := filepath.Join(root, "fake-claude")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	runtime, err := app.Open(context.Background(), filepath.Join(root, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close() })
	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Agents.Claude.Executable = executable
	socket := fmt.Sprintf("mergeyard-scheduler-test-%d", time.Now().UnixNano())
	t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
	r := runner.NewLocal(runner.Options{SocketName: socket})
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	return s, runtime, api, remote, cfg, r
}

const successfulScript = `printf '%s\n' 'implemented' > feature.txt
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"schema_version":1,"status":"success","summary":"Added feature"}}'`

func finish(t *testing.T, s *scheduler.Scheduler, state workflow.State, phase workflow.Phase) workflow.Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) == 1 && runs[0].State == state && runs[0].Phase == phase {
			return runs[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("flow did not reach %s/%s: %+v", state, phase, runs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func TestIssueToDraftPRAndRepeatedTicksAfterRestart(t *testing.T) {
	s, runtime, api, remote, cfg, r := localFlow(t, successfulScript)
	run := finish(t, s, workflow.Active, workflow.Review)
	if run.PRNumber != 101 || run.ReviewRound != 1 {
		t.Fatalf("M1 endpoint metadata: %+v", run)
	}
	if api.creations != 1 || !api.prs["mergeyard/issue-7"].Draft || !strings.Contains(api.prs["mergeyard/issue-7"].Body, "Closes #7") {
		t.Fatal("expected one draft PR with closing reference")
	}
	if got := gitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7"); got != "1" {
		t.Fatalf("implementation commits = %s", got)
	}
	if got := gitCommand(t, remote, "show", "mergeyard/issue-7:feature.txt"); got != "implemented" {
		t.Fatalf("pushed contents = %s", got)
	}
	issue := api.issues["owner/repo"][0]
	if !has(issue, "agent-running") || has(issue, "ready-for-agent") {
		t.Fatalf("claim labels = %+v", issue.Labels)
	}
	history, err := runtime.Events.History(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var transitions []string
	for _, e := range history {
		if e.Type == "run.claimed" || e.Type == "run.preparing" || e.Type == "phase.started" {
			transitions = append(transitions, e.Type)
		}
	}
	if strings.Join(transitions, ",") != "run.claimed,run.preparing,phase.started" {
		t.Fatalf("lifecycle = %v", transitions)
	}
	// Simulate an accidental ready label and a fresh control plane using the same DB.
	api.issues["owner/repo"][0].Labels = []github.Label{{Name: "ready-for-agent"}}
	restarted, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err := restarted.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := restarted.Runs(context.Background())
	if err != nil || len(runs) != 1 || runs[0].ID != run.ID || api.creations != 1 {
		t.Fatalf("duplicate run/PR after restart: %+v, %v, PRs=%d", runs, err, api.creations)
	}
	treePaths, _ := filepath.Glob(filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", "*"))
	if len(treePaths) != 1 {
		t.Fatalf("worktrees = %v", treePaths)
	}
	phasePaths, _ := filepath.Glob(filepath.Join(runtime.Workspace.Root, "runs", run.ID, "phases", "*"))
	if len(phasePaths) != 1 {
		t.Fatalf("M1 started a reviewer or duplicate implement attempt: %v", phasePaths)
	}
	input, err := os.ReadFile(filepath.Join(phasePaths[0], "input.md"))
	if err != nil || !strings.Contains(string(input), "Implement this") {
		t.Fatalf("phase input missing: %s, %v", input, err)
	}
	result, err := os.ReadFile(filepath.Join(phasePaths[0], "result.json"))
	var parsed map[string]any
	if err != nil || json.Unmarshal(result, &parsed) != nil || parsed["status"] != "success" {
		t.Fatalf("parsed phase result missing: %s, %v", result, err)
	}
}
func has(issue github.Issue, label string) bool {
	for _, l := range issue.Labels {
		if l.Name == label {
			return true
		}
	}
	return false
}

func TestImplementFailuresPreserveWorkspaceAndRequireAttention(t *testing.T) {
	cases := []struct{ name, script, code string }{
		{"blocked", `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":1,"status":"blocked","summary":"Need a product decision"}}'`, "phase.blocked"},
		{"invalid", `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"status":"success"}}'`, "phase.result_invalid"},
		{"failed", `exit 1`, "phase.retries_exhausted"},
		{"no-changes", `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":1,"status":"success","summary":"Nothing to do"}}'`, "git.no_changes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, runtime, api, remote, _, _ := localFlow(t, tc.script)
			run := finish(t, s, workflow.NeedsAttention, workflow.Implement)
			if run.LastErrorCode != tc.code || run.LastErrorMessage == "" {
				t.Fatalf("failure = %+v", run)
			}
			issue := api.issues["owner/repo"][0]
			if has(issue, "agent-running") || has(issue, "ready-for-agent") || !has(issue, "agent-needs-attention") {
				t.Fatalf("attention labels = %+v", issue.Labels)
			}
			trees, _ := filepath.Glob(filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", "*"))
			if len(trees) != 1 {
				t.Fatal("attention discarded worktree")
			}
			if api.creations != 0 || gitCommand(t, remote, "branch", "--list", "mergeyard/issue-7") != "" {
				t.Fatal("failed implementation published work")
			}
			for range 2 {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			phases, _ := filepath.Glob(filepath.Join(runtime.Workspace.Root, "runs", run.ID, "phases", "*"))
			if len(phases) != 1 {
				t.Fatal("attention retried without user action")
			}
		})
	}
}
func TestPauseContinuesExistingRunAndResumeClaimsNewIssue(t *testing.T) {
	s, runtime, api, _, _, _ := localFlow(t, successfulScript)
	if err := s.Pause(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(context.Background())
	if len(runs) != 0 {
		t.Fatal("paused scheduler claimed an issue")
	}
	if err := s.Pause(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Pause(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.Active, workflow.Review)
	// READY_TO_MERGE releases a slot; global pause still blocks new issues.
	for _, trigger := range []workflow.Trigger{workflow.ReviewApproved, workflow.CIPassed} {
		if _, err := runtime.Workflow.Transition(context.Background(), run.ID, workflow.Request{Trigger: trigger}); err != nil {
			t.Fatal(err)
		}
	}
	api.issues["owner/repo"] = append(api.issues["owner/repo"], ready(8))
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, _ = s.Runs(context.Background())
	if len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatal("paused scheduler claimed another issue")
	}
	if err := s.Pause(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, _ = s.Runs(context.Background())
	if len(runs) != 2 {
		t.Fatal("resume did not claim the newly available issue")
	}
}

type rejectingGit struct {
	prepare func(managedgit.PrepareRequest)
}

func (rejectingGit) Inspect(context.Context, managedgit.Run) error   { return nil }
func (rejectingGit) ListWorktrees(context.Context) ([]string, error) { return nil, nil }

func (g rejectingGit) Prepare(_ context.Context, req managedgit.PrepareRequest) (managedgit.Run, error) {
	if g.prepare != nil {
		g.prepare(req)
	}
	return managedgit.Run{}, &fault.Error{Code: "git.branch_conflict", Message: "Branch already belongs to someone else"}
}
func (rejectingGit) CommitAndPush(context.Context, managedgit.Run, managedgit.Phase) (managedgit.CommitResult, error) {
	panic("unexpected push")
}
func configured(t *testing.T, api *fakeGitHub, yaml string, g scheduler.Git) (*scheduler.Scheduler, *app.Runtime) {
	t.Helper()
	runtime, err := app.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close() })
	cfg, _, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Git: g})
	if err != nil {
		t.Fatal(err)
	}
	return s, runtime
}
func TestOrderingAndGlobalAndRepositoryLimits(t *testing.T) {
	older := ready(9)
	older.CreatedAt = older.CreatedAt.Add(-time.Hour)
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7), older, ready(2)}, "owner/second": {ready(3), ready(1)}, "owner/disabled": {ready(4)}}}
	var claims []string
	g := rejectingGit{prepare: func(req managedgit.PrepareRequest) {
		claims = append(claims, fmt.Sprintf("%s#%d", req.Repository, req.IssueNumber))
	}}
	s, _ := configured(t, api, "concurrency: 3\nrepositories:\n  - repo: owner/repo\n    concurrency: 2\n  - repo: owner/second\n  - repo: owner/disabled\n    enabled: false\n", g)
	for range 3 {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(claims, ","); got != "owner/repo#9,owner/repo#2,owner/second#1" {
		t.Fatalf("claim order and limits = %s", got)
	}
	// Attention still consumes capacity even though no workspace was created.
	runs, _ := s.Runs(context.Background())
	if len(runs) != 3 {
		t.Fatalf("active run slots = %d", len(runs))
	}
	for _, run := range runs {
		if run.State != workflow.NeedsAttention || run.LastErrorCode != "git.branch_conflict" {
			t.Fatalf("branch conflict outcome = %+v", run)
		}
	}
}
func TestClaimOrderAndLabelFailureAbortPreparation(t *testing.T) {
	for _, failure := range []string{"", "add", "remove"} {
		t.Run("failure-"+failure, func(t *testing.T) {
			api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}}
			var s *scheduler.Scheduler
			prepared := false
			var actions []string
			api.mutate = func(action, repo string, n int, label string) error {
				if label == "agent-needs-attention" || (action == "remove" && label == "agent-running") {
					return nil
				}
				runs, err := s.Runs(context.Background())
				if err != nil || len(runs) != 1 || runs[0].State != workflow.Claiming {
					t.Fatalf("labels changed before durable CLAIMING: %+v, %v", runs, err)
				}
				if action == "remove" && !has(api.issues[repo][0], "agent-running") {
					t.Fatal("ready removed before running was added")
				}
				actions = append(actions, action+":"+label)
				if action == failure {
					return &fault.Error{Code: "github.forbidden", Message: "Cannot mutate claim label"}
				}
				return nil
			}
			g := rejectingGit{prepare: func(req managedgit.PrepareRequest) {
				prepared = true
				runs, _ := s.Runs(context.Background())
				if runs[0].State != workflow.Preparing || has(api.issues[req.Repository][0], "ready-for-agent") || !has(api.issues[req.Repository][0], "agent-running") {
					t.Fatal("preparation began before successful claim")
				}
			}}
			s, _ = configured(t, api, "repositories:\n  - repo: owner/repo\n", g)
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if failure != "" && prepared {
				t.Fatal("failed labels allowed preparation")
			}
			if failure == "" && (!prepared || strings.Join(actions, ",") != "add:agent-running,remove:ready-for-agent") {
				t.Fatalf("claim ordering = %v, prepared=%v", actions, prepared)
			}
			runs, _ := s.Runs(context.Background())
			if runs[0].State != workflow.NeedsAttention {
				t.Fatalf("failure outcome = %+v", runs[0])
			}
		})
	}
}

func TestPhaseRetryIsBoundedAndKeepsOneRun(t *testing.T) {
	for _, success := range []bool{true, false} {
		t.Run(map[bool]string{true: "succeeds", false: "exhausted"}[success], func(t *testing.T) {
			script := `if [ ! -f retry-marker ]; then
 printf '%s\n' 'first attempt' > retry-marker
 printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":1,"status":"failed","summary":"Temporary failure"}}'
 else
 ` + successfulScript + `
 fi`
			if !success {
				script = `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":1,"status":"failed","summary":"Persistent failure"}}'`
			}
			_, runtime, api, _, cfg, r := localFlow(t, script)
			cfg.Repositories[0].Implementer.MaxAttempts = 2
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			state, phase := workflow.Active, workflow.Review
			if !success {
				state, phase = workflow.NeedsAttention, workflow.Implement
			}
			run := finish(t, s, state, phase)
			for range 3 {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			history, err := runtime.Events.History(context.Background(), 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			var attempts []int
			for _, event := range history {
				if event.RunID == run.ID && event.Type == "phase.attempt_started" {
					var payload struct {
						Attempt int `json:"attempt"`
					}
					if err := json.Unmarshal(event.Payload, &payload); err != nil {
						t.Fatal(err)
					}
					attempts = append(attempts, payload.Attempt)
				}
			}
			if len(attempts) != 2 || attempts[0] != 1 || attempts[1] != 2 {
				t.Fatalf("attempt-start notifications = %v; want [1 2]", attempts)
			}
			phases, _ := filepath.Glob(filepath.Join(runtime.Workspace.Root, "runs", run.ID, "phases", "*"))
			if len(phases) != 2 {
				t.Fatalf("phase attempts = %v", phases)
			}
			runs, _ := s.Runs(context.Background())
			if len(runs) != 1 {
				t.Fatal("retry created another run")
			}
		})
	}
}
func TestEveryNonterminalStateConsumesSlotExceptReadyToMerge(t *testing.T) {
	for _, state := range []workflow.State{workflow.Claiming, workflow.Preparing, workflow.Active, workflow.Manual, workflow.NeedsAttention, workflow.WaitingForHarness, workflow.WaitingForCI, workflow.ReadyToMerge, workflow.Completed, workflow.Failed, workflow.Stopped} {
		t.Run(string(state), func(t *testing.T) {
			api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}}
			s, runtime := configured(t, api, "repositories:\n  - repo: owner/repo\n", rejectingGit{})
			ctx := context.Background()
			w := runtime.Workflow
			requests := []workflow.Request{{Trigger: workflow.IssueClaimed, Repository: "owner/old", IssueNumber: 1}}
			if state != workflow.Claiming {
				requests = append(requests, workflow.Request{Trigger: workflow.ClaimSucceeded})
			}
			if state != workflow.Claiming && state != workflow.Preparing {
				requests = append(requests, workflow.Request{Trigger: workflow.WorktreeReady})
			}
			fail := &fault.Error{Code: "phase.blocked", Message: "Manual decision needed"}
			switch state {
			case workflow.Manual:
				requests = append(requests, workflow.Request{Trigger: workflow.TakeOver})
			case workflow.NeedsAttention:
				requests = append(requests, workflow.Request{Trigger: workflow.PhaseBlocked, Failure: fail})
			case workflow.WaitingForHarness:
				requests = append(requests, workflow.Request{Trigger: workflow.HarnessLimited})
			case workflow.WaitingForCI, workflow.ReadyToMerge, workflow.Completed:
				requests = append(requests, workflow.Request{Trigger: workflow.ImplementSucceeded}, workflow.Request{Trigger: workflow.ReviewApproved})
				if state != workflow.WaitingForCI {
					requests = append(requests, workflow.Request{Trigger: workflow.CIPassed})
				}
				if state == workflow.Completed {
					requests = append(requests, workflow.Request{Trigger: workflow.PRMerged})
				}
			case workflow.Failed:
				requests = append(requests, workflow.Request{Trigger: workflow.InternalFailure, Failure: fail})
			case workflow.Stopped:
				requests = append(requests, workflow.Request{Trigger: workflow.Stop})
			}
			for _, req := range requests {
				if _, err := w.Transition(ctx, "old-run", req); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			runs, _ := s.Runs(ctx)
			want := 1
			if state == workflow.ReadyToMerge || state.Terminal() {
				want = 2
			}
			if len(runs) != want {
				t.Fatalf("%s slot accounting produced %d runs, want %d", state, len(runs), want)
			}
		})
	}
}

type gatedGitHub struct {
	*fakeGitHub
	entered chan struct{}
	release chan struct{}
}

func (g gatedGitHub) UnresolvedBlockers(ctx context.Context, repo string, n int) ([]github.Issue, error) {
	g.entered <- struct{}{}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-g.release:
	}
	return g.fakeGitHub.UnresolvedBlockers(ctx, repo, n)
}
func TestPauseDuringEligibilityStopsPendingClaim(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}}
	_, runtime := fixture(t, api)
	cfg, _, _ := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
	g := gatedGitHub{fakeGitHub: api, entered: make(chan struct{}, 1), release: make(chan struct{})}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: g, Git: rejectingGit{}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Tick(context.Background()) }()
	select {
	case <-g.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("tick did not reach blockers")
	}
	if err := s.Pause(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	close(g.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(context.Background())
	if len(runs) != 0 {
		t.Fatal("pause returned but pending eligibility check still claimed an issue")
	}
}
func TestSchedulerRestartObservesExistingTmuxAttempt(t *testing.T) {
	script := `while [ ! -f release ]; do /bin/sleep 0.02; done
 ` + successfulScript
	s, runtime, api, _, cfg, r := localFlow(t, script)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(context.Background())
	if len(runs) != 1 || runs[0].Phase != workflow.Implement {
		t.Fatalf("running attempt = %+v", runs)
	}
	restarted, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := restarted.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	paths, _ := filepath.Glob(filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", "*"))
	if len(paths) != 1 {
		t.Fatal("duplicate worktree after restart")
	}
	if err := os.WriteFile(filepath.Join(paths[0], "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	run := finish(t, restarted, workflow.Active, workflow.Review)
	phases, _ := filepath.Glob(filepath.Join(runtime.Workspace.Root, "runs", run.ID, "phases", "*"))
	if len(phases) != 1 {
		t.Fatalf("restart duplicated running process: %v", phases)
	}
}
func TestPollingRunStopsOnCancellation(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{}}
	s, _ := fixture(t, api)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run cancellation = %v", err)
	}
	if err := s.Tick(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Tick cancellation = %v", err)
	}
}

type observingGitHub struct {
	*fakeGitHub
	polls chan struct{}
}

func (o observingGitHub) ListOpenIssues(ctx context.Context, repo string) ([]github.Issue, error) {
	select {
	case o.polls <- struct{}{}:
	default:
	}
	return o.fakeGitHub.ListOpenIssues(ctx, repo)
}
func TestRunPollsImmediatelyAndPeriodically(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{}}
	_, runtime := fixture(t, api)
	cfg, _, _ := config.Parse([]byte("poll_interval: 10ms\nrepositories:\n  - repo: owner/repo\n"))
	observed := observingGitHub{fakeGitHub: api, polls: make(chan struct{}, 4)}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: observed})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	for range 2 {
		select {
		case <-observed.polls:
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not poll")
		}
	}
	if err := s.Run(ctx); err == nil {
		t.Fatal("started concurrent polling loops")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run exit = %v", err)
	}
}

func TestConcurrentTicksProduceOneRunAndAttempt(t *testing.T) {
	s, runtime, api, _, _, _ := localFlow(t, successfulScript)
	done := make(chan error, 6)
	for range 6 {
		go func() { done <- s.Tick(context.Background()) }()
	}
	for range 6 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	run := finish(t, s, workflow.Active, workflow.Review)
	runs, _ := s.Runs(context.Background())
	phases, _ := filepath.Glob(filepath.Join(runtime.Workspace.Root, "runs", run.ID, "phases", "*"))
	if len(runs) != 1 || len(phases) != 1 || api.creations != 1 {
		t.Fatalf("concurrent ticks duplicated work: runs=%d, attempts=%d, PRs=%d", len(runs), len(phases), api.creations)
	}
}

func TestTakeoverDuringPreparationPreservesManualState(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}}
	entered := make(chan managedgit.PrepareRequest, 1)
	release := make(chan struct{})
	takeoverDone := make(chan error, 1)
	g := rejectingGit{prepare: func(req managedgit.PrepareRequest) { entered <- req; <-release }}
	s, runtime := configured(t, api, "repositories:\n  - repo: owner/repo\n", g)
	tickDone := make(chan error, 1)
	go func() { tickDone <- s.Tick(context.Background()) }()
	req := <-entered
	go func() {
		_, err := runtime.Workflow.Transition(context.Background(), req.RunID, workflow.Request{Trigger: workflow.TakeOver})
		takeoverDone <- err
	}()
	// A takeover can wait for the in-flight Git operation. If it commits early,
	// the operation must not replace MANUAL with NEEDS_ATTENTION afterwards.
	early := false
	select {
	case err := <-takeoverDone:
		early = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-tickDone; err != nil {
		t.Fatal(err)
	}
	if !early {
		if err := <-takeoverDone; err != nil {
			t.Fatal(err)
		}
	}
	run, err := runtime.Workflow.Get(context.Background(), req.RunID)
	if err != nil || run.State != workflow.Manual {
		t.Fatalf("scheduler overwrote takeover: %+v, %v", run, err)
	}
}

func schedulerResources(runtime *app.Runtime) scheduler.Resources {
	return scheduler.Resources{DB: runtime.DB, Events: runtime.Events, Workflow: runtime.Workflow, Workspace: runtime.Workspace, Control: runtime.Scheduler}
}

func TestRuntimeDashboardControlSharesDispatchGate(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}}
	s, runtime := configured(t, api, "repositories:\n  - repo: owner/repo\n", rejectingGit{})
	ctx := context.Background()
	if err := runtime.Scheduler.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(ctx)
	if len(runs) != 0 {
		t.Fatal("dashboard pause did not prevent dispatch")
	}
	if err := s.Pause(ctx, false); err != nil {
		t.Fatal(err)
	}
	if runtime.Scheduler.Paused() {
		t.Fatal("scheduler resume did not update dashboard control")
	}
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, _ = s.Runs(ctx)
	if len(runs) != 1 {
		t.Fatal("shared gate did not resume dispatch")
	}
}
