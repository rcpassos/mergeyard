package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/escalation"
	"github.com/rcpassos/mergeyard/internal/github"
)

func TestEscalationBodyPreservesSummaryAndUserContent(t *testing.T) {
	generated, err := github.GeneratePullRequestBody(github.PullRequestContent{IssueNumber: 100, IssueTitle: "Flags", Summary: "Original summary", RunID: "run"})
	if err != nil {
		t.Fatal(err)
	}
	prefix, suffix := "User intro\r\n\r\n", "\n\nUser checklist\n"
	for _, body := range []string{prefix + generated + suffix, "User-only body"} {
		snapshot := escalation.Snapshot{HeadSHA: "approved", Reasons: []escalation.Flag{{Reason: "migration", Files: []string{"migrations/schema.sql"}}}}
		updated, err := github.UpdateEscalationBody(body, snapshot)
		if err != nil || !strings.Contains(updated, "Needs careful review") || !strings.Contains(updated, "migrations/schema.sql") {
			t.Fatalf("update: %s %v", updated, err)
		}
		if strings.Contains(body, generated) && (!strings.HasPrefix(updated, prefix) || !strings.HasSuffix(updated, suffix) || !strings.Contains(updated, "Original summary")) {
			t.Fatal("lost authored content or summary")
		}
		again, err := github.UpdateEscalationBody(updated, snapshot)
		if err != nil || updated != again {
			t.Fatal("update is not idempotent")
		}
		snapshot.HeadSHA = "next-approved"
		snapshot.Reasons = []escalation.Flag{{Reason: `<script>auth</script>`, Files: []string{"auth/<script>.go", "<!-- mergeyard:generated:end -->", "<!-- mergeyard:escalation:end -->"}}}
		again, err = github.UpdateEscalationBody(updated, snapshot)
		if err != nil || strings.Contains(again, "migration") || strings.Contains(again, "<script>") || !strings.Contains(again, "&lt;script&gt;auth&lt;/script&gt;") || !strings.Contains(again, "next-approved") || strings.Count(again, "<!-- mergeyard:escalation:end -->") != 1 {
			t.Fatalf("stale or unsafe output: %s %v", again, err)
		}
		cleared, err := github.UpdateEscalationBody(again, escalation.Snapshot{})
		if err != nil || strings.Contains(cleared, "Needs careful review") || strings.Contains(cleared, "mergeyard:escalation") {
			t.Fatalf("clear: %s %v", cleared, err)
		}
		if strings.Contains(body, generated) && (!strings.HasPrefix(cleared, prefix) || !strings.HasSuffix(cleared, suffix)) {
			t.Fatal("clearing lost user text")
		}
	}
}

func TestEscalationBodyRejectsAmbiguousOwnership(t *testing.T) {
	snapshot := escalation.Snapshot{Reasons: []escalation.Flag{{Reason: "ci", Files: []string{"Dockerfile"}}}}
	for _, body := range []string{
		"<!-- mergeyard:generated:start -->missing end", "<!-- mergeyard:generated:end -->",
		"<!-- mergeyard:escalation:start -->missing end", "<!-- mergeyard:escalation:end -->",
		"<!-- mergeyard:escalation:start --><!-- mergeyard:escalation:start --><!-- mergeyard:escalation:end -->",
	} {
		if _, err := github.UpdateEscalationBody(body, snapshot); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

func TestEscalationPublicationRereadsAfterAmbiguousWrite(t *testing.T) {
	snapshot := escalation.Snapshot{HeadSHA: "approved", Reasons: []escalation.Flag{{Reason: "ci", Files: []string{"Dockerfile"}}}}
	runner := &recordedGH{responses: []response{
		{stdout: pullRequestJSON(24, false, "User intro")},
		{stderr: []byte("gh: Service Unavailable (HTTP 503)"), err: errors.New("exit status 1")},
		{stdout: pullRequestJSON(24, false, "User intro\nNew human edit")},
		{stdout: pullRequestJSON(24, false, "updated")},
	}}
	client := github.New(runner)
	if err := client.UpdateEscalation(context.Background(), "rcpassos/mergeyard", 24, snapshot); err == nil || len(runner.calls) != 2 {
		t.Fatalf("ambiguous write replayed: %v %+v", err, runner.calls)
	}
	if err := client.UpdateEscalation(context.Background(), "rcpassos/mergeyard", 24, snapshot); err != nil || len(runner.calls) != 4 {
		t.Fatalf("retry: %v %+v", err, runner.calls)
	}
	var payload struct{ Body string }
	if err := json.Unmarshal(runner.calls[3].input, &payload); err != nil || !strings.Contains(payload.Body, "New human edit") || !strings.Contains(payload.Body, "Dockerfile") {
		t.Fatalf("retry overwrote human edits: %s %v", payload.Body, err)
	}
	// The same observed body requires no PATCH after restart/ambiguous success.
	body, _ := github.UpdateEscalationBody("User intro", snapshot)
	runner = &recordedGH{responses: []response{{stdout: pullRequestJSON(24, false, body)}}}
	if err := github.New(runner).UpdateEscalation(context.Background(), "rcpassos/mergeyard", 24, snapshot); err != nil || len(runner.calls) != 1 {
		t.Fatal("already-applied update repeated")
	}
}
