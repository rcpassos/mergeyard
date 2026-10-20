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
	leftovers := github.StoppedLeftovers{Branch: "mergeyard/issue-7", Worktree: "worktrees/owner-repo/run-1", Reason: reason}
	err := github.New(r).EnsureStopLeftoversComment(context.Background(), "owner/repo", 7, "run-1", leftovers)
	if err == nil || len(r.calls) != 3 {
		t.Fatalf("unexpected retry after ambiguous POST: %v", err)
	}
	var posted struct {
		Body string `json:"body"`
	}
	if json.Unmarshal(r.calls[2].input, &posted) != nil {
		t.Fatalf("invalid comment body: %s", r.calls[2].input)
	}
	for _, want := range []string{"mergeyard/issue-7", "worktrees/owner-repo/run-1", reason, "run-1", "Mergeyard workspace"} {
		if !strings.Contains(posted.Body, want) {
			t.Fatalf("the comment does not name %q: %s", want, posted.Body)
		}
	}
	// The issue may be public, so the comment names the worktree relative to
	// the workspace and never the operator's filesystem layout.
	for _, forbidden := range []string{"/workspace", "/Users/", "/home/", "~"} {
		if strings.Contains(posted.Body, forbidden) {
			t.Fatalf("the comment exposes a local path %q: %s", forbidden, posted.Body)
		}
	}
	// The replay finds the comment instead of posting a second one.
	existing, _ := json.Marshal([]github.Comment{{ID: 1, URL: "https://github.com/owner/repo/issues/7#issuecomment-1", Body: posted.Body, User: &github.Actor{Login: "mergeyard-user"}}})
	restarted := &recordedGH{responses: []response{{stdout: []byte(`{"login":"mergeyard-user"}`)}, {stdout: existing}}}
	if err := github.New(restarted).EnsureStopLeftoversComment(context.Background(), "owner/repo", 7, "run-1", leftovers); err != nil {
		t.Fatal(err)
	}
	if len(restarted.calls) != 2 {
		t.Fatalf("the replay posted a second comment: %d requests", len(restarted.calls))
	}
}

// TestStopLeftoversCommentReplayFindsACommentInTheOldFormat proves that a
// comment already posted with an absolute path is still found by the replay, so
// the new wording never posts a second comment for the same run.
func TestStopLeftoversCommentReplayFindsACommentInTheOldFormat(t *testing.T) {
	old := github.StopLeftoversMarker + ":run-1\nMergeyard stopped run run-1 while it was preparing and kept the leftovers it could not remove:\n\n" +
		"- Local branch `mergeyard/issue-7`\n- Worktree `/workspace/worktrees/owner-repo/run-1`\n\nreason\n\nMergeyard did not delete them."
	existing, _ := json.Marshal([]github.Comment{{ID: 1, URL: "https://github.com/owner/repo/issues/7#issuecomment-1", Body: old, User: &github.Actor{Login: "mergeyard-user"}}})
	r := &recordedGH{responses: []response{{stdout: []byte(`{"login":"mergeyard-user"}`)}, {stdout: existing}}}
	leftovers := github.StoppedLeftovers{Branch: "mergeyard/issue-7", Worktree: "worktrees/owner-repo/run-1", Reason: "reason"}
	if err := github.New(r).EnsureStopLeftoversComment(context.Background(), "owner/repo", 7, "run-1", leftovers); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 2 {
		t.Fatalf("the replay posted a second comment: %d requests", len(r.calls))
	}
}

// TestStopLeftoversCommentRejectsIncompleteInput proves that an explanation
// without the run, the branch, or the worktree is never posted.
func TestStopLeftoversCommentRejectsIncompleteInput(t *testing.T) {
	r := &recordedGH{responses: []response{{stdout: []byte(`{"login":"mergeyard-user"}`)}}}
	for _, tc := range []struct {
		number           int
		run              string
		branch, worktree string
	}{
		{0, "run-1", "mergeyard/issue-7", "worktrees/owner-repo/run-1"},
		{7, "", "mergeyard/issue-7", "worktrees/owner-repo/run-1"},
		{7, "run-1", "", "worktrees/owner-repo/run-1"},
		{7, "run-1", "mergeyard/issue-7", ""},
		// An absolute path would expose the operator's filesystem layout.
		{7, "run-1", "mergeyard/issue-7", "/workspace/worktrees/owner-repo/run-1"},
	} {
		leftovers := github.StoppedLeftovers{Branch: tc.branch, Worktree: tc.worktree, Reason: "reason"}
		if err := github.New(r).EnsureStopLeftoversComment(context.Background(), "owner/repo", tc.number, tc.run, leftovers); err == nil {
			t.Fatalf("incomplete leftovers were accepted: %+v", tc)
		}
	}
}
