package github_test

import (
	"context"
	"errors"
	"github.com/rcpassos/mergeyard/internal/github"
	"reflect"
	"testing"
)

func TestCheckEvidenceIncludesLatestStatusesAndRequirements(t *testing.T) {
	r := &recordedGH{responses: []response{
		{stdout: []byte(`{"total_count":2,"check_runs":[{"name":"build","head_sha":"abc","status":"completed","conclusion":"neutral","html_url":"https://github.com/check/1","app":{"id":42}}]} {"total_count":2,"check_runs":[{"name":"optional","head_sha":"abc","status":"in_progress","conclusion":null,"app":{"id":42}}]}`)},
		{stdout: []byte(`[{"context":"legacy","state":"success","target_url":"https://example.com"},{"context":"legacy","state":"failure"}]`)},
		{stdout: []byte(`{"protected":true}`)},
		{stdout: []byte(`{"required_status_checks":{"contexts":["build"],"checks":[{"context":"build","app_id":42}]}}`)},
		{stdout: []byte(`[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"security","integration_id":9}]}}]`)},
	}}
	e, err := github.New(r).CheckEvidence(context.Background(), "owner/repo", "abc", "main")
	if err != nil || len(e.Checks) != 3 || len(e.Required) != 2 || e.Checks[0].Conclusion != "neutral" || e.Checks[2].Conclusion != "success" || e.Required[0].AppID != 42 || !e.AllowSkippedNeutral {
		t.Fatalf("evidence: %+v %v", e, err)
	}
}

func TestCheckQueriesNeverTreatErrorsOrIncompleteResponsesAsAbsence(t *testing.T) {
	valid := []response{
		{stdout: []byte(`{"total_count":0,"check_runs":[]}`)},
		{stdout: []byte(`[]`)},
		{stdout: []byte(`{"protected":false}`)},
		{stdout: []byte(`[]`)},
	}
	for _, tc := range []struct {
		name string
		step int
		bad  response
	}{
		{"checks forbidden", 0, response{stderr: []byte("Forbidden (HTTP 403)"), err: errors.New("exit 1")}},
		{"checks missing array", 0, response{stdout: []byte(`{"total_count":0}`)}},
		{"check page missing", 0, response{stdout: []byte(`{"total_count":1,"check_runs":[]}`)}},
		{"status query error", 1, response{stderr: []byte("Not Found (HTTP 404)"), err: errors.New("exit 1")}},
		{"statuses null", 1, response{stdout: []byte(`null`)}},
		{"branch protected absent", 2, response{stdout: []byte(`{}`)}},
		{"requirements forbidden", 3, response{stderr: []byte("Forbidden (HTTP 403)"), err: errors.New("exit 1")}},
		{"requirements malformed", 3, response{stdout: []byte(`[{"type":"required_status_checks","parameters":{}}]`)}},
		{"required workflows", 3, response{stdout: []byte(`[{"type":"workflows"}]`)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			responses := append([]response(nil), valid...)
			responses[tc.step] = tc.bad
			r := &recordedGH{responses: responses}
			if e, err := github.New(r).CheckEvidence(context.Background(), "owner/repo", "abc", "main"); err == nil {
				t.Fatalf("treated unknown as absence: %+v", e)
			}
		})
	}
}

func TestNoCheckEvidenceNeedsSuccessfulClassicAndRulesetQueries(t *testing.T) {
	for _, tc := range []struct {
		name       string
		protection response
		wantErr    bool
	}{
		{"no required checks", response{stdout: []byte(`{"required_status_checks":null}`)}, false},
		{"classic query forbidden", response{stderr: []byte("Forbidden (HTTP 403)"), err: errors.New("exit 1")}, true},
		{"classic query not found", response{stderr: []byte("Not Found (HTTP 404)"), err: errors.New("exit 1")}, true},
		{"missing classic field", response{stdout: []byte(`{}`)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &recordedGH{responses: []response{{stdout: []byte(`{"total_count":0,"check_runs":[]}`)}, {stdout: []byte(`[]`)}, {stdout: []byte(`{"protected":true}`)}, tc.protection, {stdout: []byte(`[]`)}}}
			e, err := github.New(r).CheckEvidence(context.Background(), "owner/repo", "abc", "main")
			if (err != nil) != tc.wantErr {
				t.Fatalf("evidence %+v error %v", e, err)
			}
		})
	}
}

func TestMarkReadyMakesOneWriteWithLiteralIdentity(t *testing.T) {
	r := &recordedGH{responses: []response{{stderr: []byte("connection lost"), err: errors.New("exit 1")}}}
	err := github.New(r).MarkReady(context.Background(), "owner/repo", 42)
	if err == nil || len(r.calls) != 1 || !reflect.DeepEqual(r.calls[0].args, []string{"pr", "ready", "42", "--repo", "owner/repo"}) {
		t.Fatalf("write replay or wrong identity: %+v %v", r.calls, err)
	}
}

func TestRulesetOnlyProtectionRequiresPositiveClassicAbsence(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		wantErr    bool
	}{
		{"absent", `{"data":{"repository":{"ref":{"branchProtectionRule":null}}}}`, false},
		{"present", `{"data":{"repository":{"ref":{"branchProtectionRule":{"id":"rule"}}}}}`, true},
		{"permission errors with partial data", `{"errors":[{"message":"forbidden"}],"data":{"repository":{"ref":{"branchProtectionRule":null}}}}`, true},
		{"missing ref", `{"data":{"repository":{"ref":null}}}`, true},
		{"missing rule field", `{"data":{"repository":{"ref":{}}}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &recordedGH{responses: []response{
				{stdout: []byte(`{"total_count":0,"check_runs":[]}`)},
				{stdout: []byte(`[]`)},
				{stdout: []byte(`{"protected":true}`)},
				{stderr: []byte("Branch not protected (HTTP 404)"), err: errors.New("exit 1")},
				{stdout: []byte(tc.data)},
				{stdout: []byte(`[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"security","integration_id":42}]}}]`)},
			}}
			e, err := github.New(r).CheckEvidence(context.Background(), "owner/repo", "abc", "main")
			if (err != nil) != tc.wantErr {
				t.Fatalf("classic absence: %+v %v", e, err)
			}
			if !tc.wantErr && (len(e.Required) != 1 || e.Required[0].Name != "security") {
				t.Fatalf("ruleset requirement lost: %+v", e)
			}
		})
	}
}

func TestPullRequestRetainsCurrentReadinessIdentity(t *testing.T) {
	r := &recordedGH{responses: []response{{stdout: []byte(`{"number":42,"html_url":"https://github.com/owner/repo/pull/42","state":"closed","merged":true,"draft":false,"head":{"ref":"mergeyard/issue-7","sha":"approved-head","repo":{"full_name":"owner/repo"}},"base":{"ref":"main"}}`)}}}
	pr, err := github.New(r).GetPullRequest(context.Background(), "owner/repo", 42)
	if err != nil || pr.Head.SHA != "approved-head" || pr.Base.Ref != "main" || !pr.Merged || pr.Draft {
		t.Fatalf("PR identity: %+v %v", pr, err)
	}
}

func TestLegacyStatusContextIdentityIsCaseInsensitive(t *testing.T) {
	r := &recordedGH{responses: []response{
		{stdout: []byte(`{"total_count":0,"check_runs":[]}`)},
		{stdout: []byte(`[{"context":"CI","state":"success","target_url":"https://example.com/latest"}] [{"context":"ci","state":"failure","target_url":"https://example.com/old"}]`)},
		{stdout: []byte(`{"protected":true}`)},
		{stdout: []byte(`{"required_status_checks":{"contexts":["ci"],"checks":[]}}`)},
		{stdout: []byte(`[]`)},
	}}
	e, err := github.New(r).CheckEvidence(context.Background(), "owner/repo", "abc", "main")
	if err != nil {
		t.Fatal(err)
	}
	passed, attention := e.Gate()
	if !passed || attention != "" || len(e.Checks) != 1 || e.Checks[0].Name != "CI" || e.Checks[0].URL != "https://example.com/latest" {
		t.Fatalf("obsolete differently-cased status blocked latest success: %+v pass=%t attention=%s", e, passed, attention)
	}
}

func TestTestMergeFailureCannotBeHiddenByHeadSuccess(t *testing.T) {
	r := &recordedGH{responses: []response{
		{stdout: []byte(`{"number":42,"html_url":"https://github.com/owner/repo/pull/42","state":"open","draft":true,"mergeable":true,"merge_commit_sha":"test-merge","head":{"ref":"mergeyard/issue-7","sha":"approved-head","repo":{"full_name":"owner/repo"}},"base":{"ref":"main","sha":"base-head"}}`)},
		{stdout: []byte(`{"total_count":0,"check_runs":[]}`)},
		{stdout: []byte(`[{"context":"ci","state":"success"}]`)},
		{stdout: []byte(`{"protected":false}`)},
		{stdout: []byte(`[]`)},
		{stdout: []byte(`{"sha":"test-merge","parents":[{"sha":"base-head"},{"sha":"approved-head"}]}`)},
		{stdout: []byte(`{"total_count":0,"check_runs":[]}`)},
		{stdout: []byte(`[{"context":"ci","state":"failure","target_url":"https://example.com/merge-failure"}]`)},
	}}
	client := github.New(r)
	pr, err := client.GetPullRequest(context.Background(), "owner/repo", 42)
	if err != nil {
		t.Fatal(err)
	}
	e, err := client.PullRequestEvidence(context.Background(), "owner/repo", *pr)
	if err != nil {
		t.Fatal(err)
	}
	passed, _ := e.Gate()
	if passed {
		t.Fatalf("failing current test-merge CI granted readiness: %+v", e)
	}
}

func TestAppBoundLegacyStatusCanPassWithVerifiedBotIdentity(t *testing.T) {
	r := &recordedGH{responses: []response{
		{stdout: []byte(`{"total_count":0,"check_runs":[]}`)},
		{stdout: []byte(`[{"context":"ci","state":"success","creator":{"id":1234,"login":"acme-ci[bot]","type":"Bot"}}]`)},
		{stdout: []byte(`{"protected":true}`)},
		{stdout: []byte(`{"required_status_checks":{"contexts":["ci"],"checks":[{"context":"ci","app_id":42}]}}`)},
		{stdout: []byte(`[]`)},
		{stdout: []byte(`{"id":42,"slug":"acme-ci"}`)},
		{stdout: []byte(`{"id":1234,"login":"acme-ci[bot]","type":"Bot"}`)},
	}}
	e, err := github.New(r).CheckEvidence(context.Background(), "owner/repo", "abc", "main")
	if err != nil {
		t.Fatal(err)
	}
	passed, attention := e.Gate()
	if !passed || attention != "" || len(e.Checks) != 1 || e.Checks[0].AppID != 42 {
		t.Fatalf("verified app status blocked: %+v pass=%t attention=%s", e, passed, attention)
	}
}

func TestAppBoundStatusCannotBorrowMatchingCheckRunSource(t *testing.T) {
	r := &recordedGH{responses: []response{
		{stdout: []byte(`{"total_count":1,"check_runs":[{"name":"ci","head_sha":"abc","status":"completed","conclusion":"success","app":{"id":42}}]}`)},
		{stdout: []byte(`[{"context":"ci","state":"success","creator":{"id":1234,"login":"other-ci[bot]","type":"Bot"}}]`)},
		{stdout: []byte(`{"protected":true}`)},
		{stdout: []byte(`{"required_status_checks":{"contexts":["ci"],"checks":[{"context":"ci","app_id":42}]}}`)},
		{stdout: []byte(`[]`)},
		{stdout: []byte(`{"id":43,"slug":"other-ci"}`)},
		{stdout: []byte(`{"id":1234,"login":"other-ci[bot]","type":"Bot"}`)},
	}}
	e, err := github.New(r).CheckEvidence(context.Background(), "owner/repo", "abc", "main")
	if err != nil {
		t.Fatal(err)
	}
	if passed, _ := e.Gate(); passed {
		t.Fatalf("legacy status from wrong app borrowed check run's source: %+v", e)
	}
}

func TestLegacyStatusAppIdentityMustBeVerified(t *testing.T) {
	for _, tc := range []struct {
		name, creator, app, bot                   string
		appFailure, botFailure, wantErr, wantPass bool
	}{
		{name: "wrong app", creator: `{"id":1234,"login":"acme-ci[bot]","type":"Bot"}`, app: `{"id":43,"slug":"acme-ci"}`, bot: `{"id":1234,"login":"acme-ci[bot]","type":"Bot"}`},
		{name: "human cannot imitate bot", creator: `{"id":1234,"login":"acme-ci[bot]","type":"User"}`, wantErr: true},
		{name: "missing creator", creator: `null`, wantErr: true},
		{name: "different bot id", creator: `{"id":1234,"login":"acme-ci[bot]","type":"Bot"}`, app: `{"id":42,"slug":"acme-ci"}`, bot: `{"id":5678,"login":"acme-ci[bot]","type":"Bot"}`, wantErr: true},
		{name: "app slug mismatch", creator: `{"id":1234,"login":"acme-ci[bot]","type":"Bot"}`, app: `{"id":42,"slug":"another-app"}`, wantErr: true},
		{name: "bot lookup forbidden", creator: `{"id":1234,"login":"acme-ci[bot]","type":"Bot"}`, app: `{"id":42,"slug":"acme-ci"}`, botFailure: true, wantErr: true},
		{name: "app lookup forbidden", creator: `{"id":1234,"login":"acme-ci[bot]","type":"Bot"}`, appFailure: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			responses := []response{
				{stdout: []byte(`{"total_count":0,"check_runs":[]}`)},
				{stdout: []byte(`[{"context":"ci","state":"success","creator":` + tc.creator + `}]`)},
				{stdout: []byte(`{"protected":true}`)},
				{stdout: []byte(`{"required_status_checks":{"contexts":["CI"],"checks":[{"context":"ci","app_id":42}]}}`)},
				{stdout: []byte(`[]`)},
			}
			appResponse := response{stdout: []byte(tc.app)}
			if tc.appFailure {
				appResponse = response{stderr: []byte("Forbidden (HTTP 403)"), err: errors.New("exit 1")}
			}
			botResponse := response{stdout: []byte(tc.bot)}
			if tc.botFailure {
				botResponse = response{stderr: []byte("Forbidden (HTTP 403)"), err: errors.New("exit 1")}
			}
			responses = append(responses, appResponse, botResponse)
			e, err := github.New(&recordedGH{responses: responses}).CheckEvidence(context.Background(), "owner/repo", "abc", "main")
			if (err != nil) != tc.wantErr {
				t.Fatalf("source verification: %+v %v", e, err)
			}
			if err == nil {
				if passed, _ := e.Gate(); passed != tc.wantPass {
					t.Fatalf("wrong app passed source gate: %+v", e)
				}
			}
		})
	}
}

func testMergePR() github.PullRequest {
	yes := true
	pr := github.PullRequest{Number: 42, State: github.Open, Mergeable: &yes, MergeCommitSHA: "test-merge"}
	pr.Head.SHA = "approved-head"
	pr.Head.Ref = "mergeyard/issue-7"
	pr.Head.Repo.FullName = "owner/repo"
	pr.Base.Ref = "main"
	pr.Base.SHA = "base-head"
	return pr
}
func TestPullRequestEvidenceRequiresCurrentMergeOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, mergeChecks, mergeStatuses string
		passed                           bool
		attention                        string
	}{
		{name: "no merge checks selects head", mergeChecks: `{"total_count":0,"check_runs":[]}`, mergeStatuses: `[]`, passed: true},
		{name: "passing merge", mergeChecks: `{"total_count":0,"check_runs":[]}`, mergeStatuses: `[{"context":"CI","state":"success"}]`, passed: true},
		{name: "pending merge", mergeChecks: `{"total_count":0,"check_runs":[]}`, mergeStatuses: `[{"context":"ci","state":"pending"}]`},
		{name: "missing merge requirement", mergeChecks: `{"total_count":0,"check_runs":[]}`, mergeStatuses: `[{"context":"optional","state":"success"}]`},
		{name: "failing merge check run", mergeChecks: `{"total_count":1,"check_runs":[{"name":"ci","head_sha":"test-merge","status":"completed","conclusion":"failure","app":{"id":42}}]}`, mergeStatuses: `[]`, attention: "ci.repair_unavailable"},
		{name: "stale merge check run", mergeChecks: `{"total_count":1,"check_runs":[{"name":"ci","head_sha":"previous-merge","status":"completed","conclusion":"success","app":{"id":42}}]}`, mergeStatuses: `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &recordedGH{responses: []response{
				{stdout: []byte(`{"total_count":0,"check_runs":[]}`)},
				{stdout: []byte(`[{"context":"ci","state":"success"}]`)},
				{stdout: []byte(`{"protected":true}`)},
				{stdout: []byte(`{"required_status_checks":{"contexts":["ci"],"checks":[]}}`)},
				{stdout: []byte(`[]`)},
				{stdout: []byte(`{"sha":"test-merge","parents":[{"sha":"base-head"},{"sha":"approved-head"}]}`)},
				{stdout: []byte(tc.mergeChecks)},
				{stdout: []byte(tc.mergeStatuses)},
			}}
			e, err := github.New(r).PullRequestEvidence(context.Background(), "owner/repo", testMergePR())
			if err != nil {
				t.Fatal(err)
			}
			passed, attention := e.Gate()
			if passed != tc.passed || attention != tc.attention || e.SHA != "approved-head" || e.MergeSHA != "test-merge" {
				t.Fatalf("merge policy: %+v pass=%t attention=%s", e, passed, attention)
			}
		})
	}
}
func TestMergeEvidenceErrorsCannotEstablishAbsence(t *testing.T) {
	for _, tc := range []struct {
		name, parents                       string
		prUnavailable, forbidden, boolFalse bool
	}{
		{name: "generation pending", prUnavailable: true},
		{name: "merge conflict", boolFalse: true},
		{name: "different base", parents: `{"sha":"test-merge","parents":[{"sha":"old-base"},{"sha":"approved-head"}]}`},
		{name: "different head", parents: `{"sha":"test-merge","parents":[{"sha":"base-head"},{"sha":"old-head"}]}`},
		{name: "different commit", parents: `{"sha":"old-merge","parents":[{"sha":"base-head"},{"sha":"approved-head"}]}`},
		{name: "merge metadata forbidden", forbidden: true},
		{name: "merge checks forbidden", parents: `{"sha":"test-merge","parents":[{"sha":"base-head"},{"sha":"approved-head"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := testMergePR()
			if tc.prUnavailable {
				pr.Mergeable = nil
			}
			if tc.boolFalse {
				*pr.Mergeable = false
			}
			parentResponse := response{stdout: []byte(tc.parents)}
			if tc.forbidden {
				parentResponse = response{stderr: []byte("Forbidden (HTTP 403)"), err: errors.New("exit 1")}
			}
			r := &recordedGH{responses: []response{
				{stdout: []byte(`{"total_count":0,"check_runs":[]}`)},
				{stdout: []byte(`[]`)},
				{stdout: []byte(`{"protected":false}`)},
				{stdout: []byte(`[]`)},
				parentResponse,
				{stderr: []byte("Forbidden (HTTP 403)"), err: errors.New("exit 1")},
			}}
			if e, err := github.New(r).PullRequestEvidence(context.Background(), "owner/repo", pr); err == nil {
				t.Fatalf("merge query failure treated as absence: %+v", e)
			}
		})
	}
}
