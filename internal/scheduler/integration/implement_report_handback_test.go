package integration_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
	// A check retry that ends with a valid blocked report supersedes the earlier
	// success for publication. The reviewer must read the same attempt the PR body
	// shows, or the review input shows one attempt's report beside another
	// attempt's evidence.
)

func TestReviewInputUsesThePublishedImplementAttempt(t *testing.T) {

	ctx := context.Background()
	implement := `printf '%s\n' 'implemented' > feature.txt
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"schema_version":4,"verification":[{"command":"implement test","outcome":"passed","excerpt":null}],"criteria":[],"human_action":null,"status":"success","summary":"Added feature"}}'`
	blocked := `printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"schema_version":4,"verification":[{"command":"retry test","outcome":"failed","excerpt":null}],"criteria":[],"human_action":{"decision":"Clarify the missing API key","files":[],"options":[]},"status":"blocked","summary":"Blocked on the API key"}}'`
	script := `if [ -f feature.txt ]; then
` + blocked + `
else
` + implement + `
fi`
	_, rt, api, remote, cfg, r := testkit.TmuxFlow(t, script)
	// The check fails on the first implementation, so the check retry ends with
	// the blocked report the user has to repair by hand.
	cfg.Repositories[0].Checks = []string{`/usr/bin/grep -q repaired feature.txt`}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
	if run.LastErrorCode != "phase.blocked" || len(run.ImplementHistory) != 2 || run.ImplementHistory[1].Report == nil {
		t.Fatalf("fixture: %+v", run)
	}
	// The user repairs the work, publishes it and opens the pull request.
	if _, err := s.Takeover(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(rt.Workspace.Root, "worktrees", "owner-repo", run.ID)
	if err := os.WriteFile(filepath.Join(tree, "feature.txt"), []byte("repaired"), 0600); err != nil {
		t.Fatal(err)
	}
	testkit.GitCommand(t, tree, "add", ".")
	testkit.GitCommand(t, tree, "commit", "-m", "manual repair")
	testkit.GitCommand(t, tree, "push", "origin", "HEAD:refs/heads/mergeyard/issue-7")
	head := testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7")
	pr := &github.PullRequest{Number: 88, State: github.Open, URL: "https://github.com/owner/repo/pull/88"}
	pr.Head.Ref = "mergeyard/issue-7"
	pr.Head.SHA = head
	pr.Head.Repo.FullName = "owner/repo"
	pr.Base.Ref = "main"
	api.Prs = map[string]*github.PullRequest{"mergeyard/issue-7": pr}
	if _, err := s.Handback(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(rt.Workspace.Root, "runs", run.ID, "phases", "review-1-1", "input.md")
	testkit.WaitForWithin(t, 30*time.Second, func() bool {
		if err := s.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		_, err := os.Stat(inputPath)
		return err == nil
	})
	current, err := rt.Workflow.Get(ctx, run.ID)
	if err != nil || current.PRNumber != 88 || current.Phase != workflow.Review {
		t.Fatalf("handback did not start the review: %+v %v", current, err)
	}
	testkit.WaitForWithin(t, 30*time.Second, func() bool {
		if err := s.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		published, err := api.GetPullRequest(ctx, current.Repository, current.PRNumber)
		return err == nil && strings.Contains(published.Body, "Blocked on the API key")
	})
	input, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(input)
	start := strings.Index(text, "## Implementer report")
	end := strings.Index(text, "## Latest verification evidence")
	if start < 0 || end < start {
		t.Fatalf("review input has no implementer report section: %s", text)
	}
	report := text[start:end]
	if !strings.Contains(report, "Blocked on the API key") || !strings.Contains(report, "retry test") {
		t.Fatalf("review input did not use the published attempt's report: %s", report)
	}
	if strings.Contains(report, "Added feature") || strings.Contains(report, "implement test") {
		t.Fatalf("review input used the superseded attempt's report: %s", report)
	}
	evidence := text[end:]
	if !strings.Contains(evidence, "retry test") || strings.Contains(evidence, "implement test") {
		t.Fatalf("review input carried another attempt's evidence: %s", evidence)
	}
	published, err := api.GetPullRequest(ctx, current.Repository, current.PRNumber)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(published.Body, "Blocked on the API key") || !strings.Contains(published.Body, "retry test") {
		t.Fatalf("pull request body does not show the published attempt: %s", published.Body)
	}
}
