package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/github"
)

type response struct {
	stdout, stderr []byte
	err            error
}
type invocation struct {
	args  []string
	input []byte
}
type recordedGH struct {
	responses []response
	calls     []invocation
}

func (r *recordedGH) Run(ctx context.Context, input []byte, args ...string) ([]byte, []byte, error) {
	r.calls = append(r.calls, invocation{append([]string(nil), args...), append([]byte(nil), input...)})
	if len(r.responses) == 0 {
		panic("unexpected gh invocation")
	}
	next := r.responses[0]
	r.responses = r.responses[1:]
	return next.stdout, next.stderr, next.err
}
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestListOpenIssues(t *testing.T) {
	runner := &recordedGH{responses: []response{{stdout: fixture(t, "open-issues")}}}
	issues, err := github.New(runner).ListOpenIssues(context.Background(), "rcpassos/mergeyard")
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 2 {
		t.Fatalf("got %d issues, want 2", len(issues))
	}
	issue := issues[1]
	if issue.Number != 6 || issue.Title != "[M1] GitHub adapter: issues, blockers, labels" || issue.State != github.Open || issue.CreatedAt != time.Date(2026, 10, 3, 21, 25, 53, 0, time.UTC) || !reflect.DeepEqual(issue.Labels, []github.Label{{Name: "ready-for-agent"}}) {
		t.Fatalf("unexpected issue: %+v", issue)
	}
	if issue.Body != "Blocked by: #2\n\n### Context\nPRD §7 (GitHub contract), §8 (eligibility).\n\n### Scope\nThin wrapper over `gh`, with JSON output parsed into typed structs:\n- list open issues for a repository with labels, creation time, and body;\n- read unresolved blockers from GitHub's native issue dependencies (`issue_dependencies_summary.blocked_by` / the dependencies endpoints);\n- add / remove labels;\n- read a single issue's state;\n- `github.*` error codes, bounded retry with backoff for transient failures (§22).\n\nUntrusted issue text is never passed through a shell (§32).\n\n### Acceptance criteria\n- Unit tests with recorded `gh` JSON fixtures, including issues with open and closed blockers.\n- An integration test (opt-in, against a dedicated test repository) lists issues, reads blockers, and adds/removes a label." {
		t.Fatalf("issue body was changed: %q", issue.Body)
	}
}

func TestUnresolvedBlockersUsesNativeRelationships(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want []int
	}{
		{"open", fixture(t, "open-blockers"), []int{3}},
		{"closed", fixture(t, "closed-blockers"), []int{}},
		{"mixed pages", append(fixture(t, "closed-blockers"), fixture(t, "open-blockers")...), []int{3}},
		{"none", []byte("[]"), []int{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordedGH{responses: []response{{stdout: tc.data}}}
			blockers, err := github.New(runner).UnresolvedBlockers(context.Background(), "rcpassos/mergeyard", 7)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]int, 0, len(blockers))
			for _, blocker := range blockers {
				got = append(got, blocker.Number)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("blockers = %v, want %v", got, tc.want)
			}
			if !contains(runner.calls[0].args, "repos/rcpassos/mergeyard/issues/7/dependencies/blocked_by?per_page=100") {
				t.Fatalf("not reading native dependencies: %v", runner.calls)
			}
		})
	}
}
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestIssueState(t *testing.T) {
	runner := &recordedGH{responses: []response{{stdout: fixture(t, "closed-issue")}}}
	state, err := github.New(runner).IssueState(context.Background(), "rcpassos/mergeyard", 2)
	if err != nil {
		t.Fatal(err)
	}
	if state != github.Closed {
		t.Fatalf("state = %q, want closed", state)
	}
}

func TestLabelsPreserveLiteralText(t *testing.T) {
	label := "--help, $(touch /tmp/mergeyard-injected); `id` / ? # % café"
	runner := &recordedGH{responses: []response{{stdout: []byte("[]")}, {stdout: []byte("[]")}}}
	client := github.New(runner)
	if err := client.AddLabel(context.Background(), "rcpassos/mergeyard", 6, label); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal(runner.calls[0].input, &payload); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(payload.Labels, []string{label}) {
		t.Fatalf("label changed: %#v", payload.Labels)
	}
	if !contains(runner.calls[0].args, "POST") || !contains(runner.calls[0].args, "--input") {
		t.Fatalf("expected JSON stdin: %v", runner.calls[0])
	}
	if err := client.RemoveLabel(context.Background(), "rcpassos/mergeyard", 6, label); err != nil {
		t.Fatal(err)
	}
	wantPath := "repos/rcpassos/mergeyard/issues/6/labels/--help%2C%20$%28touch%20%2Ftmp%2Fmergeyard-injected%29%3B%20%60id%60%20%2F%20%3F%20%23%20%25%20caf%C3%A9"
	if !contains(runner.calls[1].args, "DELETE") || !contains(runner.calls[1].args, wantPath) {
		t.Fatalf("expected escaped label path: %v", runner.calls[1])
	}
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var ghErr *github.Error
	if !errors.As(err, &ghErr) || ghErr.Code != code {
		t.Fatalf("error = %v, want %s", err, code)
	}
}
func TestGitHubErrorCodesAndRetryBound(t *testing.T) {
	cases := []struct {
		name, stderr, code string
		transient          bool
	}{
		{"unauthenticated", "gh: Bad credentials (HTTP 401)", "github.not_logged_in", false},
		{"forbidden", "gh: Resource not accessible by integration (HTTP 403)", "github.forbidden", false},
		{"not found", "gh: Not Found (HTTP 404)", "github.not_found", false},
		{"invalid mutation", "gh: Validation Failed (HTTP 422)", "github.command_failed", false},
		{"server error", "gh: Internal Server Error (HTTP 500)", "github.unavailable", true},
		{"bad gateway", "gh: Bad Gateway (HTTP 502)", "github.unavailable", true},
		{"rate limited", "gh: API rate limit exceeded (HTTP 403)", "github.rate_limited", true},
		{"secondary limit", "gh: Too Many Requests (HTTP 429)", "github.rate_limited", true},
		{"network", "error connecting to api.github.com", "github.unavailable", true},
		{"unknown", "unexpected gh failure", "github.command_failed", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cause := errors.New("exit status 1")
			failure := response{stderr: []byte(tc.stderr), err: cause}
			count := 1
			if tc.transient {
				count = 3
			}
			runner := &recordedGH{responses: make([]response, count)}
			for i := range runner.responses {
				runner.responses[i] = failure
			}
			started := time.Now()
			_, err := github.New(runner).IssueState(context.Background(), "rcpassos/mergeyard", 2)
			requireCode(t, err, tc.code)
			if !errors.Is(err, cause) {
				t.Fatalf("lost cause: %v", err)
			}
			if len(runner.calls) != count {
				t.Fatalf("attempts = %d, want %d", len(runner.calls), count)
			}
			if tc.transient && time.Since(started) < 750*time.Millisecond {
				t.Fatal("retries did not back off")
			}
		})
	}
}
func TestTransientFailureRecovers(t *testing.T) {
	runner := &recordedGH{responses: []response{
		{stderr: []byte("gh: Service Unavailable (HTTP 503)"), err: errors.New("exit status 1")},
		{stdout: fixture(t, "closed-issue")},
	}}
	state, err := github.New(runner).IssueState(context.Background(), "rcpassos/mergeyard", 2)
	if err != nil || state != github.Closed {
		t.Fatalf("state = %q, err = %v", state, err)
	}
}
func TestCancellationStopsBackoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	runner := &recordedGH{responses: []response{{stderr: []byte("gh: Bad Gateway (HTTP 502)"), err: errors.New("exit status 1")}}}
	_, err := github.New(runner).IssueState(ctx, "rcpassos/mergeyard", 2)
	requireCode(t, err, "github.canceled")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lost context error: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatal("ran gh after cancellation")
	}
}
func TestIssueBodyNeverBecomesCommandInput(t *testing.T) {
	data := strings.ReplaceAll(string(fixture(t, "open-issues")), "Blocked by: #2", "$(touch /tmp/mergeyard-injected); `id`")
	runner := &recordedGH{responses: []response{{stdout: []byte(data)}}}
	issues, err := github.New(runner).ListOpenIssues(context.Background(), "rcpassos/mergeyard")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(issues[1].Body, "$(touch /tmp/mergeyard-injected)") {
		t.Fatal("body not preserved")
	}
	for _, call := range runner.calls {
		if len(call.input) > 0 || strings.Contains(strings.Join(call.args, " "), "touch") {
			t.Fatal("body reached gh arguments or stdin")
		}
	}
}

func TestInvalidResponsesFailClosed(t *testing.T) {
	valid := string(fixture(t, "open-blockers"))
	cases := []struct{ name, data string }{
		{"empty", ""},
		{"null", "null"},
		{"object", "{}"},
		{"missing fields", "[{}]"},
		{"invalid JSON", "[{"},
		{"invalid later page", "[] {}"},
		{"unknown state", strings.ReplaceAll(valid, `"state": "open"`, `"state": "unknown"`)},
		{"zero number", strings.ReplaceAll(valid, `"number": 3`, `"number": 0`)},
		{"missing creation time", strings.ReplaceAll(valid, `"created_at": "2026-10-03T21:25:48Z"`, `"created_at": null`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordedGH{responses: []response{{stdout: []byte(tc.data)}}}
			_, err := github.New(runner).UnresolvedBlockers(context.Background(), "rcpassos/mergeyard", 7)
			requireCode(t, err, "github.invalid_response")
		})
	}
	for _, data := range []string{"", "null", "{}", `{"state":"mystery"}`, `{"state":"open"}`} {
		t.Run("single "+data, func(t *testing.T) {
			runner := &recordedGH{responses: []response{{stdout: []byte(data)}}}
			_, err := github.New(runner).IssueState(context.Background(), "rcpassos/mergeyard", 7)
			requireCode(t, err, "github.invalid_response")
		})
	}
}
func TestPaginationExcludesPullRequestsAndClosedIssues(t *testing.T) {
	data := append(fixture(t, "open-issues"), []byte(`[ {"number": 19, "state": "open", "pull_request": {"url": "https://api.github.com/repos/rcpassos/mergeyard/pulls/19"}} ]`)...)
	data = append(data, fixture(t, "closed-blockers")...)
	runner := &recordedGH{responses: []response{{stdout: data}}}
	issues, err := github.New(runner).ListOpenIssues(context.Background(), "rcpassos/mergeyard")
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 2 {
		t.Fatalf("got %d issues, want 2", len(issues))
	}
	if !contains(runner.calls[0].args, "--paginate") {
		t.Fatal("issue listing truncated at first page")
	}
}
func TestInvalidInputsDoNotInvokeGH(t *testing.T) {
	runner := &recordedGH{}
	client := github.New(runner)
	for _, repo := range []string{"", "owner", "owner/repo/extra", "owner/repo?state=all", "../repo", "owner/..", "owner/.", "--help/repo"} {
		_, err := client.ListOpenIssues(context.Background(), repo)
		requireCode(t, err, "github.invalid_input")
	}
	for _, n := range []int{-1, 0} {
		_, err := client.IssueState(context.Background(), "rcpassos/mergeyard", n)
		requireCode(t, err, "github.invalid_input")
		_, err = client.UnresolvedBlockers(context.Background(), "rcpassos/mergeyard", n)
		requireCode(t, err, "github.invalid_input")
		requireCode(t, client.AddLabel(context.Background(), "rcpassos/mergeyard", n, "ready"), "github.invalid_input")
		requireCode(t, client.RemoveLabel(context.Background(), "rcpassos/mergeyard", n, "ready"), "github.invalid_input")
	}
	requireCode(t, client.AddLabel(context.Background(), "rcpassos/mergeyard", 6, ""), "github.invalid_input")
	requireCode(t, client.RemoveLabel(context.Background(), "rcpassos/mergeyard", 6, ""), "github.invalid_input")
	if len(runner.calls) != 0 {
		t.Fatal("invalid input invoked gh")
	}
}
func TestLabelFailureIsReturned(t *testing.T) {
	for _, remove := range []bool{false, true} {
		runner := &recordedGH{responses: []response{{stderr: []byte("gh: Forbidden (HTTP 403)"), err: errors.New("exit status 1")}}}
		client := github.New(runner)
		var err error
		if remove {
			err = client.RemoveLabel(context.Background(), "rcpassos/mergeyard", 6, "ready")
		} else {
			err = client.AddLabel(context.Background(), "rcpassos/mergeyard", 6, "running")
		}
		requireCode(t, err, "github.forbidden")
	}
}
func TestCanceledContextDoesNotInvokeGH(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := &recordedGH{}
	_, err := github.New(runner).ListOpenIssues(ctx, "rcpassos/mergeyard")
	requireCode(t, err, "github.canceled")
	if len(runner.calls) != 0 {
		t.Fatal("canceled request invoked gh")
	}
}
