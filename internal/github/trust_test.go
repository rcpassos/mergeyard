package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/github"
)

func trustIssue(t *testing.T) github.Issue {
	t.Helper()
	var edits struct {
		Data struct {
			Repository struct {
				Issue struct {
					Body string `json:"body"`
				} `json:"issue"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(fixture(t, "trust-content-edits"), &edits); err != nil {
		t.Fatal(err)
	}
	return github.Issue{Number: 105, State: github.Open, Body: edits.Data.Repository.Issue.Body, Labels: []github.Label{{Name: "ready-for-agent"}}, UpdatedAt: time.Date(2026, 10, 12, 9, 1, 48, 0, time.UTC)}
}

func TestRecordedTrustAuthorizationAndQuietPollCache(t *testing.T) {
	runner := &recordedGH{responses: []response{{stdout: fixture(t, "trust-label-events")}, {stdout: fixture(t, "trust-admin-permission")}, {stdout: fixture(t, "trust-content-edits")}}}
	client := github.New(runner)
	issue := trustIssue(t)
	for range 4 {
		decision, err := client.VerifyIssueTrust(context.Background(), "rcpassos/mergeyard", issue, "ready-for-agent", nil)
		if err != nil || !decision.Allowed || decision.LabelEventID != 32789751593 {
			t.Fatalf("authorization: %+v, %v", decision, err)
		}
	}
	if len(runner.calls) != 3 {
		t.Fatalf("quiet issue made %d requests; want 3 total", len(runner.calls))
	}
	if !strings.Contains(strings.Join(runner.calls[0].args, " "), "--paginate") {
		t.Fatal("label events must read every page")
	}
}

func TestDefaultActorPermissions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		allowed bool
		data    []byte
	}{
		{"admin recorded", true, fixture(t, "trust-admin-permission")},
		{"read recorded", false, fixture(t, "trust-read-permission")},
		{"write", true, []byte(`{"permission":"write"}`)},
		{"maintain", true, []byte(`{"permission":"maintain"}`)},
		{"triage", false, []byte(`{"permission":"triage"}`)},
		{"legacy push", true, []byte(`{"permission":"push"}`)},
		{"legacy pull", false, []byte(`{"permission":"pull"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordedGH{responses: []response{{stdout: tc.data}}}
			allowed, err := github.New(runner).IsTrustedActor(context.Background(), "rcpassos/mergeyard", "actor", nil)
			if err != nil || allowed != tc.allowed {
				t.Fatalf("trusted=%v, err=%v", allowed, err)
			}
		})
	}
}

func TestExplicitActorTrustReplacesPermissions(t *testing.T) {
	runner := &recordedGH{}
	client := github.New(runner)
	for _, tc := range []struct {
		login   string
		allowed bool
	}{{"Maintainer", true}, {"another-admin", false}, {"", false}, {"evil/actor", false}} {
		allowed, err := client.IsTrustedActor(context.Background(), "owner/repo", tc.login, []string{"maintainer"})
		if err != nil || allowed != tc.allowed {
			t.Fatalf("%s: trusted=%v, err=%v", tc.login, allowed, err)
		}
	}
	if len(runner.calls) != 0 {
		t.Fatal("explicit trust must not look up permissions")
	}
}

func editResponse(body string, edits []map[string]any, last any, editor any, next bool, cursor string) []byte {
	data, _ := json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"issue": map[string]any{
		"body": body, "lastEditedAt": last, "editor": editor, "userContentEdits": map[string]any{"nodes": edits, "pageInfo": map[string]any{"hasNextPage": next, "endCursor": cursor}},
	}}}})
	return data
}
func contentEdit(at, login string) map[string]any {
	return map[string]any{"editedAt": at, "editor": map[string]any{"login": login}}
}

func TestBodyEditsAfterAuthorization(t *testing.T) {
	issue := trustIssue(t)
	for _, tc := range []struct {
		name    string
		edits   []map[string]any
		editor  any
		last    any
		allowed bool
		reason  string
	}{
		{"untrusted after", []map[string]any{contentEdit("2026-10-09T00:00:00Z", "outsider")}, map[string]any{"login": "outsider"}, "2026-10-09T00:00:00Z", false, "@outsider"},
		{"trusted after", []map[string]any{contentEdit("2026-10-09T00:00:00Z", "rcpassos")}, map[string]any{"login": "rcpassos"}, "2026-10-09T00:00:00Z", true, ""},
		{"untrusted before", []map[string]any{contentEdit("2026-10-08T13:02:58Z", "outsider")}, map[string]any{"login": "outsider"}, "2026-10-08T13:02:58Z", true, ""},
		{"hidden editor", []map[string]any{contentEdit("2026-10-09T00:00:00Z", "rcpassos")}, nil, "2026-10-09T00:00:00Z", false, "latest"},
		{"trusted latest does not bless earlier untrusted edit", []map[string]any{contentEdit("2026-10-10T00:00:00Z", "rcpassos"), contentEdit("2026-10-09T00:00:00Z", "outsider")}, map[string]any{"login": "rcpassos"}, "2026-10-10T00:00:00Z", false, "@outsider"},
		{"same second fails closed", []map[string]any{contentEdit("2026-10-08T13:07:09Z", "outsider")}, map[string]any{"login": "outsider"}, "2026-10-08T13:07:09Z", false, "@outsider"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordedGH{responses: []response{{stdout: fixture(t, "trust-label-events")}, {stdout: editResponse(issue.Body, tc.edits, tc.last, tc.editor, false, "")}}}
			decision, err := github.New(runner).VerifyIssueTrust(context.Background(), "rcpassos/mergeyard", issue, "ready-for-agent", []string{"rcpassos"})
			if err != nil || decision.Allowed != tc.allowed || !strings.Contains(decision.Reason, tc.reason) {
				t.Fatalf("decision: %+v, %v", decision, err)
			}
		})
	}
}

func TestTrustReadsLaterEditPages(t *testing.T) {
	issue := trustIssue(t)
	editor := map[string]any{"login": "rcpassos"}
	runner := &recordedGH{responses: []response{
		{stdout: fixture(t, "trust-label-events")},
		{stdout: editResponse(issue.Body, []map[string]any{contentEdit("2026-10-10T00:00:00Z", "rcpassos")}, "2026-10-10T00:00:00Z", editor, true, "page2")},
		{stdout: editResponse(issue.Body, []map[string]any{contentEdit("2026-10-09T00:00:00Z", "outsider")}, "2026-10-10T00:00:00Z", editor, false, "")},
	}}
	decision, err := github.New(runner).VerifyIssueTrust(context.Background(), "rcpassos/mergeyard", issue, "ready-for-agent", []string{"rcpassos"})
	if err != nil || decision.Allowed || !strings.Contains(decision.Reason, "@outsider") {
		t.Fatalf("decision: %+v, %v", decision, err)
	}
	var request struct {
		Variables struct {
			Cursor string `json:"cursor"`
		} `json:"variables"`
	}
	if json.Unmarshal(runner.calls[2].input, &request) != nil || request.Variables.Cursor != "page2" {
		t.Fatal("edit pagination cursor not sent")
	}
}

func TestUntrustedLatestLabelAndRoleSelections(t *testing.T) {
	for _, label := range []string{"ready-for-agent", "mergeyard:implementer=codex", "mergeyard:reviewer=claude", "mergeyard:implementer-tier=medium", "MERGEYARD:REVIEWER-TIER=HIGH", "mergeyard:implementer-tier=unknown"} {
		t.Run(label, func(t *testing.T) {
			issue := trustIssue(t)
			if label != "ready-for-agent" {
				issue.Labels = append(issue.Labels, github.Label{Name: label})
			}
			later := `[{"id":40000000000,"event":"labeled","actor":{"login":"outsider"},"label":{"name":"` + label + `"},"created_at":"2026-10-09T00:00:00Z"}]`
			runner := &recordedGH{responses: []response{{stdout: append(fixture(t, "trust-label-events"), []byte(later)...)}, {stdout: fixture(t, "trust-content-edits")}}}
			decision, err := github.New(runner).VerifyIssueTrust(context.Background(), "rcpassos/mergeyard", issue, "ready-for-agent", []string{"rcpassos"})
			if err != nil || decision.Allowed || !strings.Contains(decision.Reason, label) || !strings.Contains(decision.Reason, "@outsider") {
				t.Fatalf("decision: %+v, %v", decision, err)
			}
		})
	}
}

func TestTrustCacheInvalidatesOnIssueAndPolicyChanges(t *testing.T) {
	issue := trustIssue(t)
	runner := &recordedGH{responses: []response{{stdout: fixture(t, "trust-label-events")}, {stdout: fixture(t, "trust-content-edits")}}}
	client := github.New(runner)
	decision, err := client.VerifyIssueTrust(context.Background(), "rcpassos/mergeyard", issue, "ready-for-agent", []string{"rcpassos"})
	if err != nil || !decision.Allowed {
		t.Fatalf("initial: %+v %v", decision, err)
	}
	issue.UpdatedAt = issue.UpdatedAt.Add(time.Second)
	runner.responses = []response{{stdout: fixture(t, "trust-label-events")}, {stdout: editResponse(issue.Body, []map[string]any{contentEdit("2026-10-12T10:00:00Z", "outsider")}, "2026-10-12T10:00:00Z", map[string]any{"login": "outsider"}, false, "")}}
	decision, err = client.VerifyIssueTrust(context.Background(), "rcpassos/mergeyard", issue, "ready-for-agent", []string{"rcpassos"})
	if err != nil || decision.Allowed {
		t.Fatalf("edit reused approval: %+v %v", decision, err)
	}
	runner.responses = []response{{stdout: fixture(t, "trust-label-events")}}
	decision, err = client.VerifyIssueTrust(context.Background(), "rcpassos/mergeyard", issue, "ready-for-agent", []string{"different-maintainer"})
	if err != nil || decision.Allowed || !strings.Contains(decision.Reason, "Label") {
		t.Fatalf("policy reused decision: %+v %v", decision, err)
	}
}

func TestUnreadableHistoryNeverAuthorizes(t *testing.T) {
	for _, data := range []string{`{"errors":[{"message":"forbidden"}]}`, `{"data":{"repository":{"issue":null}}}`, `{"data":{"repository":{"issue":{"body":"","userContentEdits":null}}}}`, `not json`} {
		runner := &recordedGH{responses: []response{{stdout: fixture(t, "trust-label-events")}, {stdout: []byte(data)}}}
		decision, err := github.New(runner).VerifyIssueTrust(context.Background(), "rcpassos/mergeyard", trustIssue(t), "ready-for-agent", []string{"rcpassos"})
		if err != nil || decision.Allowed || decision.Reason == "" {
			t.Fatalf("unreadable authorized: %+v %v", decision, err)
		}
	}
}

func TestRefusalCommentReplayAfterAmbiguousWrite(t *testing.T) {
	runner := &recordedGH{responses: []response{{stdout: []byte(`{"login":"mergeyard-user"}`)}, {stdout: []byte(`[]`)}, {stderr: []byte("connection reset"), err: errors.New("failed")}}}
	client := github.New(runner)
	err := client.EnsureTrustRefusalComment(context.Background(), "owner/repo", 7, "Untrusted actor.")
	if err == nil || len(runner.calls) != 3 {
		t.Fatalf("ambiguous comment must not retry POST: %v", err)
	}
	var posted struct {
		Body string `json:"body"`
	}
	if json.Unmarshal(runner.calls[2].input, &posted) != nil {
		t.Fatal("invalid post")
	}
	existing, _ := json.Marshal([]github.Comment{{ID: 1, URL: "https://github.com/owner/repo/issues/7#issuecomment-1", Body: posted.Body, User: &github.Actor{Login: "mergeyard-user"}}})
	runner.responses = []response{{stdout: append([]byte("[]"), existing...)}}
	if err := client.EnsureTrustRefusalComment(context.Background(), "owner/repo", 7, "A different reason."); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 4 {
		t.Fatal("existing marker must prevent duplicate comment")
	}
}

func TestMalformedRelevantLabelEventFailsClosed(t *testing.T) {
	for _, label := range []string{"ready-for-agent", "mergeyard:implementer-tier=low", "MERGEYARD:REVIEWER-TIER=HIGH"} {
		t.Run(label, func(t *testing.T) {
			issue := trustIssue(t)
			later := `[{"id":40000000000,"event":"labeled","actor":{"login":"outsider"},"label":{"name":"` + label + `"}}]`
			runner := &recordedGH{responses: []response{{stdout: append(fixture(t, "trust-label-events"), []byte(later)...)}, {stdout: fixture(t, "trust-content-edits")}}}
			decision, err := github.New(runner).VerifyIssueTrust(context.Background(), "rcpassos/mergeyard", issue, "ready-for-agent", []string{"rcpassos"})
			if err != nil || decision.Allowed || decision.Reason == "" {
				t.Fatalf("malformed new label must not reuse older authorization: %+v %v", decision, err)
			}
		})
	}
}

func TestTierTrustRecheckRejectsReappliedCaseVariant(t *testing.T) {
	issue := trustIssue(t)
	issue.Labels = append(issue.Labels, github.Label{Name: "mergeyard:implementer-tier=high"}, github.Label{Name: "MERGEYARD:IMPLEMENTER-TIER=HIGH"})
	trustedEvent := `[{"id":40000000000,"event":"labeled","actor":{"login":"rcpassos"},"label":{"name":"mergeyard:implementer-tier=high"},"created_at":"2026-10-09T00:00:00Z"}]`
	runner := &recordedGH{responses: []response{{stdout: append(fixture(t, "trust-label-events"), []byte(trustedEvent)...)}, {stdout: fixture(t, "trust-content-edits")}}}
	client := github.New(runner)
	decision, err := client.VerifyIssueTrust(context.Background(), "rcpassos/mergeyard", issue, "ready-for-agent", []string{"rcpassos"})
	if err != nil || !decision.Allowed {
		t.Fatalf("trusted repeated tier: %+v %v", decision, err)
	}
	untrustedEvent := `[{"id":40000000001,"event":"labeled","actor":{"login":"outsider"},"label":{"name":"MERGEYARD:IMPLEMENTER-TIER=HIGH"},"created_at":"2026-10-12T09:01:48Z"}]`
	runner.responses = []response{{stdout: append(fixture(t, "trust-label-events"), []byte(trustedEvent[:len(trustedEvent)-1]+","+untrustedEvent[1:])...)}}
	decision, err = client.RecheckIssueTrust(context.Background(), "rcpassos/mergeyard", issue, "ready-for-agent", []string{"rcpassos"})
	if err != nil || decision.Allowed || !strings.Contains(decision.Reason, "@outsider") {
		t.Fatalf("claim reused cached tier authorization: %+v %v", decision, err)
	}
}

func TestClaimRecheckSeesNewLabelEvenWithIdenticalIssueSnapshot(t *testing.T) {
	issue := trustIssue(t)
	runner := &recordedGH{responses: []response{{stdout: fixture(t, "trust-label-events")}, {stdout: fixture(t, "trust-content-edits")}}}
	client := github.New(runner)
	decision, err := client.VerifyIssueTrust(context.Background(), "rcpassos/mergeyard", issue, "ready-for-agent", []string{"rcpassos"})
	if err != nil || !decision.Allowed {
		t.Fatalf("initial approval: %+v %v", decision, err)
	}
	later := `[{"id":40000000000,"event":"labeled","actor":{"login":"outsider"},"label":{"name":"ready-for-agent"},"created_at":"2026-10-12T09:01:48Z"}]`
	runner.responses = []response{{stdout: append(fixture(t, "trust-label-events"), []byte(later)...)}}
	decision, err = client.RecheckIssueTrust(context.Background(), "rcpassos/mergeyard", issue, "ready-for-agent", []string{"rcpassos"})
	if err != nil || decision.Allowed || !strings.Contains(decision.Reason, "@outsider") {
		t.Fatalf("claim reused stale label actor: %+v %v", decision, err)
	}
}

func TestIncompleteEditPaginationFailsClosed(t *testing.T) {
	issue := trustIssue(t)
	content := editResponse(issue.Body, []map[string]any{}, nil, nil, false, "")
	var payload map[string]any
	if err := json.Unmarshal(content, &payload); err != nil {
		t.Fatal(err)
	}
	payload["data"].(map[string]any)["repository"].(map[string]any)["issue"].(map[string]any)["userContentEdits"].(map[string]any)["pageInfo"] = map[string]any{}
	content, _ = json.Marshal(payload)
	runner := &recordedGH{responses: []response{{stdout: fixture(t, "trust-label-events")}, {stdout: content}}}
	decision, err := github.New(runner).VerifyIssueTrust(context.Background(), "rcpassos/mergeyard", issue, "ready-for-agent", []string{"rcpassos"})
	if err != nil || decision.Allowed || decision.Reason == "" {
		t.Fatalf("incomplete history authorized: %+v %v", decision, err)
	}
}

type refusalGH struct {
	calls    []invocation
	comments []byte
	posts    int
}

func (r *refusalGH) Run(_ context.Context, input []byte, args ...string) ([]byte, []byte, error) {
	r.calls = append(r.calls, invocation{args: args, input: input})
	if args[5] == "user" {
		return []byte(`{"login":"mergeyard-user"}`), nil, nil
	}
	if args[2] == "GET" {
		return r.comments, nil, nil
	}
	r.posts++
	var body struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(input, &body); err != nil {
		return nil, nil, err
	}
	data, _ := json.Marshal(map[string]any{"id": 99, "html_url": "https://github.com/owner/repo/issues/7#issuecomment-99", "body": body.Body, "user": map[string]any{"login": "mergeyard-user"}})
	return data, nil, nil
}

func TestCopiedTrustMarkersCannotSuppressExplanation(t *testing.T) {
	for _, copied := range []string{
		`[{"id":1,"html_url":"url","user":{"login":"outsider"},"body":"<!-- mergeyard:trust:refused -->"}]`,
		`[{"id":1,"html_url":"url","user":{"login":"outsider"},"body":"<!-- mergeyard:trust:refused -->\nMergeyard refused to claim this issue: fake."},{"id":2,"html_url":"url","user":{"login":"outsider"},"body":"<!-- mergeyard:trust:refused -->"}]`,
	} {
		runner := &refusalGH{comments: []byte(copied)}
		err := github.New(runner).EnsureTrustRefusalComment(context.Background(), "owner/repo", 7, "Untrusted actor.")
		if err != nil || runner.posts != 1 {
			t.Fatalf("copied marker suppressed explanation: posts=%d err=%v", runner.posts, err)
		}
	}
}
