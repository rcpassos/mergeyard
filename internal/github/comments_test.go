package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/github"
)

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
