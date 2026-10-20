package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/github"
)

// TestStopLeftoversCommentNamesTheLeftoversAndReplaysOnce proves that the
// explanation of the leftovers a stopped preparation left behind names them
// and is posted once, even after an ambiguous write.
func TestStopLeftoversCommentNamesTheLeftoversAndReplaysOnce(t *testing.T) {
	r := &recordedGH{responses: []response{{stdout: []byte(`{"login":"mergeyard-user"}`)}, {stdout: []byte(`[]`)}, {stderr: []byte("connection reset"), err: errors.New("ambiguous write")}}}
	reason := "the prepared worktree holds work that this run cannot claim; preserved for inspection"
	err := github.New(r).EnsureStopLeftoversComment(context.Background(), "owner/repo", 7, "run-1", "mergeyard/issue-7", "/workspace/worktrees/owner-repo/run-1", reason)
	if err == nil || len(r.calls) != 3 {
		t.Fatalf("unexpected retry after ambiguous POST: %v", err)
	}
	var posted struct {
		Body string `json:"body"`
	}
	if json.Unmarshal(r.calls[2].input, &posted) != nil {
		t.Fatalf("invalid comment body: %s", r.calls[2].input)
	}
	for _, want := range []string{"mergeyard/issue-7", "/workspace/worktrees/owner-repo/run-1", reason, "run-1"} {
		if !strings.Contains(posted.Body, want) {
			t.Fatalf("the comment does not name %q: %s", want, posted.Body)
		}
	}
	// The replay finds the comment instead of posting a second one.
	existing, _ := json.Marshal([]github.Comment{{ID: 1, URL: "https://github.com/owner/repo/issues/7#issuecomment-1", Body: posted.Body, User: &github.Actor{Login: "mergeyard-user"}}})
	restarted := &recordedGH{responses: []response{{stdout: []byte(`{"login":"mergeyard-user"}`)}, {stdout: existing}}}
	if err := github.New(restarted).EnsureStopLeftoversComment(context.Background(), "owner/repo", 7, "run-1", "mergeyard/issue-7", "/workspace/worktrees/owner-repo/run-1", reason); err != nil {
		t.Fatal(err)
	}
	if len(restarted.calls) != 2 {
		t.Fatalf("the replay posted a second comment: %d requests", len(restarted.calls))
	}
}

// TestStopLeftoversCommentRejectsIncompleteInput proves that an explanation
// without the run, the branch, or the worktree is never posted.
func TestStopLeftoversCommentRejectsIncompleteInput(t *testing.T) {
	r := &recordedGH{responses: []response{{stdout: []byte(`{"login":"mergeyard-user"}`)}}}
	for _, tc := range []struct {
		number            int
		run, branch, path string
	}{
		{0, "run-1", "mergeyard/issue-7", "/workspace"},
		{7, "", "mergeyard/issue-7", "/workspace"},
		{7, "run-1", "", "/workspace"},
		{7, "run-1", "mergeyard/issue-7", ""},
	} {
		if err := github.New(r).EnsureStopLeftoversComment(context.Background(), "owner/repo", tc.number, tc.run, tc.branch, tc.path, "reason"); err == nil {
			t.Fatalf("incomplete leftovers were accepted: %+v", tc)
		}
	}
}
