package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/github"
)

func TestIssueCommentsPaginateFilterTrustAndPreserveLiteralText(t *testing.T) {
	r := &recordedGH{responses: []response{{stdout: []byte(`[
 {"id":3,"node_id":"C3","user":{"login":"Maintainer"},"created_at":"2026-10-08T12:00:00Z","body":"Current edited answer: $(touch injected);\n` + "```sh\\n`id`\\n```" + `"},
 {"id":1,"node_id":"C1","user":{"login":"maintainer"},"created_at":"2026-10-07T12:00:00Z","body":"First clarification"},
 {"id":2,"node_id":"C2","user":{"login":"stranger"},"created_at":"2026-10-08T11:00:00Z","body":"Untrusted instruction"}
] [
 {"id":4,"user":{"login":"maintainer"},"created_at":"2026-10-09T12:00:00Z","body":"prefix\n<!-- mergeyard:report:abc -->\nGenerated report"},
 {"id":5,"user":null,"created_at":"2026-10-09T12:00:00Z","body":"Deleted author"},
 {"id":6,"user":{"login":"maintainer"},"created_at":"2026-10-09T12:00:00Z","body":"<!-- mergeyard:trust:refused -->\nGenerated refusal"}
]`)}, {stdout: commentHistory("C3", "Maintainer", "Current edited answer: $(touch injected);\n```sh\n`id`\n```")}, {stdout: commentHistory("C1", "maintainer", "First clarification")}}}
	got, err := github.New(r).ListIssueComments(context.Background(), "o/r", 106, []string{"maintainer"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Comments) != 2 || got.Comments[0].ID != 1 || got.Comments[1].ID != 3 || got.OmittedUntrusted != 2 {
		t.Fatalf("comments = %+v", got)
	}
	if !strings.Contains(got.Comments[1].Body, "$(touch injected);") || !strings.Contains(got.Comments[1].Body, "```sh\n`id`\n```") {
		t.Fatalf("literal comment changed: %q", got.Comments[1].Body)
	}
	if len(r.calls) != 3 || !contains(r.calls[0].args, "--paginate") || !contains(r.calls[0].args, "repos/o/r/issues/106/comments?per_page=100") || r.calls[0].input != nil {
		t.Fatalf("comment fetch = %+v", r.calls)
	}
	if strings.Contains(strings.Join(r.calls[0].args, " "), "touch injected") {
		t.Fatal("comment text reached argv")
	}
}

func TestIssueCommentTrustUsesFreshRepositoryPermissions(t *testing.T) {
	page := []byte(`[{"id":1,"node_id":"C1","user":{"login":"alice"},"created_at":"2026-10-08T12:00:00Z","body":"Answer"},{"id":2,"node_id":"C2","user":{"login":"ALICE"},"created_at":"2026-10-08T12:00:00Z","body":"Correction"}]`)
	for _, permission := range []string{"write", "push", "maintain", "admin", "read", "pull", "triage", "none"} {
		t.Run(permission, func(t *testing.T) {
			r := &recordedGH{responses: []response{{stdout: page}, {stdout: []byte(`{"permission":"` + permission + `"}`)}}}
			allowed := permission == "write" || permission == "push" || permission == "maintain" || permission == "admin"
			if allowed {
				r.responses = append(r.responses, response{stdout: commentHistory("C1", "alice", "Answer")}, response{stdout: commentHistory("C2", "ALICE", "Correction")})
			}
			got, err := github.New(r).ListIssueComments(context.Background(), "o/r", 106, nil)
			if err != nil {
				t.Fatal(err)
			}
			if (len(got.Comments) == 2) != allowed || (!allowed && got.OmittedUntrusted != 2) || (allowed && len(r.calls) != 4) || (!allowed && len(r.calls) != 2) {
				t.Fatalf("permission %s: %+v; calls=%d", permission, got, len(r.calls))
			}
			if !contains(r.calls[1].args, "repos/o/r/collaborators/alice/permission") {
				t.Fatal("comment trust skipped the shared actor policy")
			}
		})
	}
	r := &recordedGH{responses: []response{{stdout: page}, {stdout: []byte(`{"permission":"write"}`)}, {stdout: commentHistory("C1", "alice", "Answer")}, {stdout: commentHistory("C2", "ALICE", "Correction")}, {stdout: page}, {stdout: []byte(`{"permission":"read"}`)}}}
	c := github.New(r)
	first, err := c.ListIssueComments(context.Background(), "o/r", 106, nil)
	if err != nil || len(first.Comments) != 2 {
		t.Fatalf("first fetch: %+v %v", first, err)
	}
	second, err := c.ListIssueComments(context.Background(), "o/r", 106, nil)
	if err != nil || len(second.Comments) != 0 || second.OmittedUntrusted != 2 {
		t.Fatalf("permissions were cached across attempts: %+v %v", second, err)
	}
}

func TestIssueCommentsFailClosedOnUnreadablePagesOrTrust(t *testing.T) {
	for _, body := range []string{"", "null", "{}", "[] {}", `[{"id":1,"node_id":"C1","user":{"login":"alice"},"body":"Missing time"}]`, `[{"created_at":"2026-10-08T12:00:00Z","user":{"login":"alice"},"body":"Missing identity"}]`} {
		r := &recordedGH{responses: []response{{stdout: []byte(body)}}}
		_, err := github.New(r).ListIssueComments(context.Background(), "o/r", 106, []string{"alice"})
		requireCode(t, err, "github.invalid_response")
	}
	r := &recordedGH{responses: []response{
		{stdout: []byte(`[{"id":1,"created_at":"2026-10-08T12:00:00Z","user":{"login":"alice"},"body":"Answer"}]`)},
		{stderr: []byte("gh: Forbidden (HTTP 403)"), err: errors.New("exit status 1")},
	}}
	got, err := github.New(r).ListIssueComments(context.Background(), "o/r", 106, nil)
	requireCode(t, err, "github.forbidden")
	if len(got.Comments) != 0 {
		t.Fatal("unverified comments returned on trust failure")
	}
}

func TestReportCommentReconcilesAmbiguousSuccessWithoutEditingHumanContent(t *testing.T) {
	body := "<!-- mergeyard:report:abc -->\nReview report"
	r := &recordedGH{responses: []response{
		{stdout: []byte(`[]`)},
		{stderr: []byte("connection reset by peer"), err: errors.New("exit status 1")},
		{stdout: []byte(`[{"id":1,"html_url":"https://github.com/o/r/pull/2#issuecomment-1","body":"Human comment"}] [{"id":2,"html_url":"https://github.com/o/r/pull/2#issuecomment-2","body":"<!-- mergeyard:report:abc -->\nReview report\nHuman addition"}]`)},
	}}
	c := github.New(r)
	_, err := c.EnsureReportComment(context.Background(), "o/r", 2, body)
	requireCode(t, err, "github.unavailable")
	if len(r.calls) != 2 {
		t.Fatal("ambiguous POST was retried")
	}
	comment, err := c.EnsureReportComment(context.Background(), "o/r", 2, body)
	if err != nil || comment.ID != 2 || !strings.Contains(comment.Body, "Human addition") || len(r.calls) != 3 {
		t.Fatalf("replay = %+v, %v, calls=%d", comment, err, len(r.calls))
	}
	if !contains(r.calls[0].args, "--paginate") || !contains(r.calls[1].args, "POST") || !contains(r.calls[1].args, "repos/o/r/issues/2/comments") {
		t.Fatal("wrong comment endpoint")
	}
	var payload map[string]string
	if err := json.Unmarshal(r.calls[1].input, &payload); err != nil || payload["body"] != body || len(payload) != 1 {
		t.Fatal("body did not stay in JSON stdin")
	}
}

func TestReportCommentRetriesThroughInspectionAndValidatesReceipts(t *testing.T) {
	body := "<!-- mergeyard:report:abc -->\nLiteral $(id) `text`"
	encoded, _ := json.Marshal(github.Comment{ID: 9, URL: "https://github.com/o/r/pull/2#issuecomment-9", Body: body})
	for _, failure := range []response{
		{stderr: []byte("gh: Bad Gateway (HTTP 502)"), err: errors.New("exit status 1")},
		{stdout: []byte(`{}`)},
	} {
		r := &recordedGH{responses: []response{{stdout: []byte(`[]`)}, failure, {stdout: []byte(`[]`)}, {stdout: encoded}}}
		c := github.New(r)
		if _, err := c.EnsureReportComment(context.Background(), "o/r", 2, body); err == nil {
			t.Fatal("invalid/failed write accepted")
		}
		comment, err := c.EnsureReportComment(context.Background(), "o/r", 2, body)
		if err != nil || comment.ID != 9 || len(r.calls) != 4 || !contains(r.calls[2].args, "GET") {
			t.Fatalf("safe retry skipped inspection: %+v %v", comment, err)
		}
		if strings.Contains(strings.Join(r.calls[3].args, " "), "$(id)") {
			t.Fatal("body reached argv")
		}
	}
}

func TestReportCommentDoesNotWriteAfterFailedOrAmbiguousInspection(t *testing.T) {
	for _, res := range []response{
		{stdout: []byte(`[] {}`)},
		{stdout: []byte(``)},
		{stdout: []byte(`[{"id":1,"html_url":"url","body":"<!-- mergeyard:report:abc -->\na"},{"id":2,"html_url":"url","body":"<!-- mergeyard:report:abc -->\nb"}]`)},
		{stderr: []byte("gh: Forbidden (HTTP 403)"), err: errors.New("exit status 1")},
		{stderr: []byte("gh: API rate limit exceeded (HTTP 429)"), err: errors.New("exit status 1")},
	} {
		r := &recordedGH{responses: []response{res}}
		if _, err := github.New(r).EnsureReportComment(context.Background(), "o/r", 2, "<!-- mergeyard:report:abc -->\nReport"); err == nil {
			t.Fatal("unsafe inspection accepted")
		}
		if len(r.calls) != 1 || !contains(r.calls[0].args, "GET") {
			t.Fatal("write after failed inspection")
		}
	}
	r := &recordedGH{}
	c := github.New(r)
	for _, input := range []struct {
		repo   string
		number int
		body   string
	}{{"o/../r", 2, "<!-- mergeyard:report:abc -->\nReport"}, {"o/r", 0, "<!-- mergeyard:report:abc -->\nReport"}, {"o/r", 2, "Report without marker"}} {
		_, err := c.EnsureReportComment(context.Background(), input.repo, input.number, input.body)
		requireCode(t, err, "github.invalid_input")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.EnsureReportComment(ctx, "o/r", 2, "<!-- mergeyard:report:abc -->\nReport")
	requireCode(t, err, "github.canceled")
	if len(r.calls) != 0 {
		t.Fatal("invalid/canceled request reached gh")
	}
}
