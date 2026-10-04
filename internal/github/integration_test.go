package github_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/github"
)

func TestGitHubPullRequestIntegration(t *testing.T) {
	if os.Getenv("MERGEYARD_GITHUB_PR_INTEGRATION") != "1" {
		t.Skip("opt in with MERGEYARD_GITHUB_PR_INTEGRATION=1; see testdata/README.md")
	}
	repo := os.Getenv("MERGEYARD_GITHUB_TEST_REPO")
	if !strings.HasSuffix(repo, "/mergeyard-integration-test") {
		t.Fatal("MERGEYARD_GITHUB_TEST_REPO must name a dedicated owner/mergeyard-integration-test repository")
	}
	number, err := strconv.Atoi(os.Getenv("MERGEYARD_GITHUB_TEST_ISSUE"))
	if err != nil || number <= 0 {
		t.Fatal("MERGEYARD_GITHUB_TEST_ISSUE must be a positive issue number")
	}
	head, base := os.Getenv("MERGEYARD_GITHUB_TEST_HEAD"), os.Getenv("MERGEYARD_GITHUB_TEST_BASE")
	if !strings.HasPrefix(head, "mergeyard-integration/") || base == "" || head == base {
		t.Fatal("prepare a pushed mergeyard-integration/<unique> branch and set MERGEYARD_GITHUB_TEST_BASE")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client := github.New(nil)
	pr, err := client.FindOpenPullRequest(ctx, repo, head)
	if err != nil {
		t.Fatal(err)
	}
	if pr != nil {
		t.Fatal("test branch already has an open PR; refusing to modify it")
	}
	content := github.PullRequestContent{IssueNumber: number, IssueTitle: "Mergeyard PR integration test", Summary: "Initial integration summary.", RunID: fmt.Sprintf("integration-%d", time.Now().UnixNano())}
	// Register cleanup before creation: an interrupted response may hide success.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		created, err := client.FindOpenPullRequest(cleanupCtx, repo, head)
		if err != nil {
			t.Errorf("cleanup: find test PR for %s: %v", head, err)
			return
		}
		if created != nil {
			if err := integrationPRPatch(cleanupCtx, repo, created.Number, map[string]string{"state": "closed"}); err != nil {
				t.Errorf("cleanup: close test PR #%d manually: %v", created.Number, err)
			}
		}
	})
	created, err := client.CreateDraftPullRequest(ctx, repo, head, base, content)
	if err != nil {
		t.Fatal(err)
	}
	if !created.Draft {
		t.Fatal("live draft test requires a repository that supports drafts")
	}
	found, err := client.FindOpenPullRequest(ctx, repo, head)
	if err != nil || found == nil || found.Number != created.Number || found.URL != created.URL {
		t.Fatalf("rediscovered PR = %+v, error = %v", found, err)
	}
	prefix, suffix := "User introduction.\n\n", "\n\nUser checklist: keep this text.\n"
	if err := integrationPRPatch(ctx, repo, created.Number, map[string]string{"body": prefix + found.Body + suffix}); err != nil {
		t.Fatal(err)
	}
	content.Summary = "Updated integration summary."
	updated, err := client.UpdatePullRequest(ctx, repo, created.Number, content)
	if err != nil {
		t.Fatal(err)
	}
	found, err = client.FindOpenPullRequest(ctx, repo, head)
	if err != nil || found == nil {
		t.Fatalf("updated PR missing: %v", err)
	}
	if found.Body != updated.Body || !found.Draft || !strings.HasPrefix(found.Body, prefix) || !strings.HasSuffix(found.Body, suffix) || !strings.Contains(found.Body, "Updated integration summary.") || strings.Contains(found.Body, "Initial integration summary.") || !strings.Contains(found.Body, "Closes #"+strconv.Itoa(number)) || !strings.Contains(found.Body, content.RunID) {
		t.Fatalf("updated body or draft state incorrect: %+v", found)
	}
}

// Simulate a human body edit, or close the test PR, using the same JSON stdin rule.
func integrationPRPatch(ctx context.Context, repo string, number int, fields map[string]string) error {
	input, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "gh", "api", "--method", "PATCH", fmt.Sprintf("repos/%s/pulls/%d", repo, number), "--input", "-")
	cmd.Stdin = bytes.NewReader(input)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh: %s: %w", output, err)
	}
	return nil
}

// This test never derives its repository from a user's Mergeyard config or git remote.
func TestGitHubIntegration(t *testing.T) {
	if os.Getenv("MERGEYARD_GITHUB_INTEGRATION") != "1" {
		t.Skip("opt in with MERGEYARD_GITHUB_INTEGRATION=1; see testdata/README.md")
	}
	repo := os.Getenv("MERGEYARD_GITHUB_TEST_REPO")
	if !strings.HasSuffix(repo, "/mergeyard-integration-test") {
		t.Fatal("MERGEYARD_GITHUB_TEST_REPO must name a dedicated owner/mergeyard-integration-test repository")
	}
	number, err := strconv.Atoi(os.Getenv("MERGEYARD_GITHUB_TEST_ISSUE"))
	if err != nil || number <= 0 {
		t.Fatal("MERGEYARD_GITHUB_TEST_ISSUE must be a positive issue number")
	}
	const label = "mergeyard-integration-test"
	client := github.New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	issues, err := client.ListOpenIssues(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	target := findIssue(t, issues, number)
	if hasLabel(target, label) {
		t.Fatal("test issue already has the test label; refusing to remove an existing label")
	}
	state, err := client.IssueState(ctx, repo, number)
	if err != nil || state != github.Open {
		t.Fatalf("state = %q, error = %v", state, err)
	}
	blockers, err := client.UnresolvedBlockers(ctx, repo, number)
	if err != nil {
		t.Fatal(err)
	}
	for _, blocker := range blockers {
		if blocker.State != github.Open {
			t.Fatalf("resolved blocker returned: %+v", blocker)
		}
	}
	// Register cleanup before the mutation: an interrupted request might have applied it.
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if err := client.RemoveLabel(cleanupCtx, repo, number, label); err != nil {
			t.Errorf("cleanup: remove test label manually: %v", err)
		}
	})
	if err := client.AddLabel(ctx, repo, number, label); err != nil {
		t.Fatal(err)
	}
	issues, err = client.ListOpenIssues(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if !hasLabel(findIssue(t, issues, number), label) {
		t.Fatal("added label missing from GitHub")
	}
	if err := client.RemoveLabel(ctx, repo, number, label); err != nil {
		t.Fatal(err)
	}
	issues, err = client.ListOpenIssues(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if hasLabel(findIssue(t, issues, number), label) {
		t.Fatal("removed label still present on GitHub")
	}
}
func findIssue(t *testing.T, issues []github.Issue, number int) github.Issue {
	t.Helper()
	for _, issue := range issues {
		if issue.Number == number {
			return issue
		}
	}
	t.Fatalf("open test issue #%d not found", number)
	return github.Issue{}
}
func hasLabel(issue github.Issue, name string) bool {
	for _, label := range issue.Labels {
		if label.Name == name {
			return true
		}
	}
	return false
}
