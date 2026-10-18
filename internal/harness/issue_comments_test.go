package harness_test

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func writeCommentInput(t *testing.T, phase harness.PhaseContext, input harness.ImplementInput) string {
	t.Helper()
	r := runner.NewLocal(runner.Options{})
	var path string
	var err error
	switch phase.Phase {
	case workflow.Implement:
		path, err = harness.WriteImplementInput(context.Background(), r, phase, input)
	case workflow.Fix:
		path, err = harness.WriteFixInput(context.Background(), r, phase, harness.FixInput{Issue: input})
	case workflow.Review:
		path, err = harness.WriteReviewInput(context.Background(), r, phase, harness.ReviewInput{Issue: input})
	}
	if err != nil {
		t.Fatal(err)
	}
	data, err := r.ReadFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestAllPhaseInputsIncludeLiteralCommentsOnResume(t *testing.T) {
	for _, p := range []workflow.Phase{workflow.Implement, workflow.Fix, workflow.Review} {
		t.Run(string(p), func(t *testing.T) {
			phase := phase(t)
			phase.Phase = p
			body := "Answer: $(touch injected); 'quoted'\n```sh\n`id`\n```"
			input := harness.ImplementInput{Comments: github.IssueComments{
				Comments: []github.Comment{
					{ID: 1, User: &github.Actor{Login: "alice"}, CreatedAt: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC), Body: "Earlier clarification"},
					{ID: 2, User: &github.Actor{Login: "bob"}, CreatedAt: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), Body: body},
				}, OmittedUntrusted: 3,
			}}
			text := writeCommentInput(t, phase, input)
			for _, want := range []string{"## Issue comments", "@alice", "@bob", "2026-10-08T12:00:00Z", body, "3 untrusted comments omitted"} {
				if !strings.Contains(text, want) {
					t.Fatalf("missing %q in %s", want, text)
				}
			}
			if strings.Index(text, "Earlier clarification") > strings.Index(text, body) {
				t.Fatal("comments are not oldest first")
			}
			for _, adapter := range []harness.HarnessAdapter{harness.NewClaude(config.Claude{}), harness.NewCodex(config.Codex{})} {
				built, err := adapter.BuildInvocation(phase, config.Role{})
				if err != nil || strings.Contains(strings.Join(built.Command.Args, " "), "touch injected") {
					t.Fatalf("comment reached %s command: %+v %v", adapter.Type(), built, err)
				}
			}
			phase.Resume = true
			input.Comments.Comments[1].Body = "Updated answer on resume"
			text = writeCommentInput(t, phase, input)
			if !strings.Contains(text, "Updated answer on resume") || strings.Contains(text, "touch injected") {
				t.Fatalf("stale resumed input: %s", text)
			}
		})
	}
}

func TestIssueCommentInputKeepsNewestWithin32KiB(t *testing.T) {
	input := harness.ImplementInput{Comments: github.IssueComments{Comments: []github.Comment{
		{User: &github.Actor{Login: "old"}, Body: strings.Repeat("a", 20000)},
		{User: &github.Actor{Login: "middle"}, Body: strings.Repeat("b", 20000)},
		{User: &github.Actor{Login: "new"}, Body: strings.Repeat("c", 10000)},
	}}}
	text := writeCommentInput(t, phase(t), input)
	if strings.Contains(text, "@old") || !strings.Contains(text, strings.Repeat("b", 20000)) || !strings.Contains(text, strings.Repeat("c", 10000)) || !strings.Contains(text, "Truncated") || !strings.Contains(text, "32768") {
		t.Fatal("cap did not retain the newest comments and disclose truncation")
	}
	input.Comments.Comments = []github.Comment{{User: &github.Actor{Login: "new"}, Body: "old prefix" + strings.Repeat("界", 12000) + "new suffix"}}
	text = writeCommentInput(t, phase(t), input)
	if !utf8.ValidString(text) || !strings.Contains(text, "new suffix") || strings.Contains(text, "old prefix") || !strings.Contains(text, "Truncated") || strings.Count(text, "界") > 10919 {
		t.Fatal("oversized newest comment was not clipped safely within the cap")
	}
}
