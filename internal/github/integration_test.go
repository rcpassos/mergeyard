package github_test

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/github"
)

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
