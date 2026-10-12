package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/github"
)

func TestHarnessOverrideCommentReplayAfterAmbiguousWriteAndRestart(t *testing.T) {
	r := &recordedGH{responses: []response{{stdout: []byte(`{"login":"mergeyard-user"}`)}, {stdout: []byte(`[]`)}, {stderr: []byte("connection reset"), err: errors.New("ambiguous write")}}}
	reason := "Invalid labels: mergeyard:implementer=unknown. Valid values: claude, codex."
	if err := github.New(r).EnsureHarnessOverrideComment(context.Background(), "owner/repo", 7, reason); err == nil || len(r.calls) != 3 {
		t.Fatalf("unexpected retry after ambiguous POST: %v", err)
	}
	var posted struct {
		Body string `json:"body"`
	}
	if json.Unmarshal(r.calls[2].input, &posted) != nil || !strings.Contains(posted.Body, reason) {
		t.Fatal("invalid explanation")
	}
	// A copied marker from another author cannot suppress the actual explanation.
	for _, login := range []string{"mergeyard-user", "outsider"} {
		existing, _ := json.Marshal([]github.Comment{{ID: 1, URL: "https://github.com/owner/repo/issues/7#issuecomment-1", Body: posted.Body, User: &github.Actor{Login: login}}})
		restarted := &recordedGH{responses: []response{{stdout: []byte(`{"login":"mergeyard-user"}`)}, {stdout: append([]byte("[]"), existing...)}}}
		if login == "outsider" {
			created, _ := json.Marshal(github.Comment{ID: 2, URL: "https://github.com/owner/repo/issues/7#issuecomment-2", Body: posted.Body, User: &github.Actor{Login: "mergeyard-user"}})
			restarted.responses = append(restarted.responses, response{stdout: created})
		}
		if err := github.New(restarted).EnsureHarnessOverrideComment(context.Background(), "owner/repo", 7, reason); err != nil {
			t.Fatal(err)
		}
		want := 2
		if login == "outsider" {
			want = 3
		}
		if len(restarted.calls) != want {
			t.Fatalf("marker author %s: %d requests, want %d", login, len(restarted.calls), want)
		}
	}
}
