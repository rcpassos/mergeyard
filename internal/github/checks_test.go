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
