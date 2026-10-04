package scheduler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// Scope polling to the explicitly supplied test issue. All other operations use
// real GitHub, Git, tmux, SQLite, and the native Claude structured-output adapter.
type testIssueGitHub struct {
	*github.Client
	number int
}

func (c testIssueGitHub) ListOpenIssues(ctx context.Context, repo string) ([]github.Issue, error) {
	issues, err := c.Client.ListOpenIssues(ctx, repo)
	if err != nil {
		return nil, err
	}
	for _, issue := range issues {
		if issue.Number == c.number {
			return []github.Issue{issue}, nil
		}
	}
	return nil, nil
}
func TestGitHubSchedulerIntegration(t *testing.T) {
	if os.Getenv("MERGEYARD_SCHEDULER_INTEGRATION") != "1" {
		t.Skip("opt in with MERGEYARD_SCHEDULER_INTEGRATION=1; see docs/scheduler.md")
	}
	repo := os.Getenv("MERGEYARD_GITHUB_TEST_REPO")
	number, err := strconv.Atoi(os.Getenv("MERGEYARD_GITHUB_TEST_ISSUE"))
	if !strings.HasSuffix(repo, "/mergeyard-integration-test") || err != nil || number <= 0 {
		t.Fatal("supply a dedicated owner/mergeyard-integration-test repository and a positive test issue number")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client := github.New(nil)
	api := testIssueGitHub{Client: client, number: number}
	issues, err := api.ListOpenIssues(ctx, repo)
	if err != nil || len(issues) != 1 {
		t.Fatalf("test issue unavailable: %v", err)
	}
	original := issues[0]
	if !has(original, "ready-for-agent") || has(original, "agent-running") || has(original, "agent-needs-attention") {
		t.Fatal("test issue must have only the ready claim label")
	}
	blockers, err := api.UnresolvedBlockers(ctx, repo, number)
	if err != nil || len(blockers) != 0 {
		t.Fatalf("test issue must be unblocked: %+v, %v", blockers, err)
	}
	branch := fmt.Sprintf("mergeyard/issue-%d", number)
	remote := "https://github.com/" + repo + ".git"
	preflight := exec.CommandContext(ctx, "git", "ls-remote", "--heads", remote, "refs/heads/"+branch)
	if out, err := preflight.CombinedOutput(); err != nil || len(bytes.TrimSpace(out)) != 0 {
		t.Fatalf("test branch must not exist: %s, %v", out, err)
	}
	pr, err := api.FindOpenPullRequest(ctx, repo, branch)
	if err != nil || pr != nil {
		t.Fatalf("test PR must not exist: %+v, %v", pr, err)
	}
	runtime, err := app.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close() })
	root := t.TempDir()
	executable := filepath.Join(root, "fake-claude")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' 'scheduler integration change' > mergeyard-scheduler-integration-%d.txt\nprintf '%%s\\n' '%s'\n", time.Now().UnixNano(), `{"type":"result","is_error":false,"structured_output":{"schema_version":1,"status":"success","summary":"Added integration test file"}}`)
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: " + repo + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Agents.Claude.Executable = executable
	socket := fmt.Sprintf("mergeyard-live-test-%d", time.Now().UnixNano())
	r := runner.NewLocal(runner.Options{SocketName: socket})
	t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
	// Register cleanup before dispatch: an interrupted API response may hide a PR.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		found, err := client.FindOpenPullRequest(cleanupCtx, repo, branch)
		if err != nil {
			t.Errorf("cleanup: find test PR: %v", err)
			return
		}
		if found != nil {
			input, _ := json.Marshal(map[string]string{"state": "closed"})
			cmd := exec.CommandContext(cleanupCtx, "gh", "api", "--method", "PATCH", fmt.Sprintf("repos/%s/pulls/%d", repo, found.Number), "--input", "-")
			cmd.Stdin = bytes.NewReader(input)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("cleanup: close PR: %s, %v", out, err)
				return
			}
		}
		for _, label := range []string{"agent-running", "agent-needs-attention"} {
			if err := client.RemoveLabel(cleanupCtx, repo, number, label); err != nil {
				t.Errorf("cleanup label: %v", err)
			}
		}
		if err := client.AddLabel(cleanupCtx, repo, number, "ready-for-agent"); err != nil {
			t.Errorf("restore ready: %v", err)
		}
		paths, _ := filepath.Glob(filepath.Join(runtime.Workspace.Root, "worktrees", "*", "*"))
		if len(paths) == 1 {
			cmd := exec.CommandContext(cleanupCtx, "git", "push", "--no-force", "--", remote, ":refs/heads/"+branch)
			cmd.Dir = paths[0]
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("cleanup remote branch: %s, %v", out, err)
			}
		}
	})
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	var run workflow.Run
	var baseSHA string
	for {
		if err := s.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != 1 {
			t.Fatalf("expected exactly one run, got %+v", runs)
		}
		run = runs[0]
		if baseSHA == "" && run.State == workflow.Active && run.Phase == workflow.Implement {
			paths, _ := filepath.Glob(filepath.Join(runtime.Workspace.Root, "worktrees", "*", "*"))
			if len(paths) != 1 {
				t.Fatal("missing implementation worktree")
			}
			baseSHA = gitCommand(t, paths[0], "rev-parse", "HEAD")
		}
		if run.State == workflow.Active && run.Phase == workflow.Review {
			break
		}
		if run.State == workflow.NeedsAttention {
			t.Fatalf("flow requires attention: %+v", run)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	for range 3 {
		if err := s.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	pr, err = client.FindOpenPullRequest(ctx, repo, branch)
	if err != nil || pr == nil || !pr.Draft || pr.Number != run.PRNumber {
		t.Fatalf("expected one draft PR: %+v, %v", pr, err)
	}
	t.Logf("Created test draft PR: %s", pr.URL)
	issues, err = api.ListOpenIssues(ctx, repo)
	if err != nil || len(issues) != 1 || !has(issues[0], "agent-running") || has(issues[0], "ready-for-agent") {
		t.Fatalf("claim labels not applied: %+v, %v", issues, err)
	}
	trees, _ := filepath.Glob(filepath.Join(runtime.Workspace.Root, "worktrees", "*", "*"))
	if len(trees) != 1 {
		t.Fatalf("expected one worktree, got %v", trees)
	}
	// Compare with the base observed before the scheduler committed the implementation.
	if got := gitCommand(t, trees[0], "log", "-1", "--format=%s"); got != "mergeyard: #"+strconv.Itoa(number)+" "+original.Title {
		t.Fatalf("implementation commit: %s", got)
	}
	if got := gitCommand(t, trees[0], "rev-list", "--count", baseSHA+"..HEAD"); got != "1" {
		t.Fatalf("implementation commits: %s", got)
	}
	runs, _ := s.Runs(ctx)
	if len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatal("repeated ticks created duplicate runs")
	}
}
