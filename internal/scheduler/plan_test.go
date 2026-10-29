package scheduler_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/store"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestPlanMatchesNextPoll(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, yaml, label, decision, reason string
		active, paused, limited             bool
		trustErr                            bool
	}{
		{name: "eligible", decision: "would_claim"},
		{name: "blocked", decision: "skipped", reason: "blocked_by"},
		{name: "running", label: "agent-running", decision: "skipped", reason: "running_label"},
		{name: "attention", label: "agent-needs-attention", decision: "skipped", reason: "needs_attention_label"},
		{name: "active", active: true, decision: "skipped", reason: "active_run"},
		{name: "disabled", yaml: "    enabled: false\n", decision: "skipped", reason: "repo_disabled"},
		{name: "global capacity", active: true, yaml: "    concurrency: 2\n", decision: "waiting", reason: "concurrency_full"},
		{name: "repository capacity", active: true, yaml: "    concurrency: 1\n", decision: "waiting", reason: "concurrency_full"},
		{name: "limited", limited: true, decision: "waiting", reason: "harness_limited"},
		{name: "untrusted", decision: "skipped", reason: "untrusted_labeler"},
		{name: "unverifiable", trustErr: true, decision: "skipped", reason: "untrusted_labeler"},
		{name: "invalid override", label: "mergeyard:implementer=invalid", decision: "skipped", reason: "invalid_harness_override"},
		{name: "paused", paused: true, decision: "waiting", reason: "scheduler_paused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			issue := testkit.Ready(7)
			if tc.label != "" {
				issue.Labels = append(issue.Labels, github.Label{Name: tc.label})
			}
			api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {issue}}}
			if tc.name == "blocked" {
				api.Blockers = map[int][]github.Issue{7: {testkit.Ready(99), testkit.Ready(42)}}
			}
			if tc.name == "untrusted" || tc.trustErr {
				api.Trust = func(github.Issue) (github.TrustDecision, error) {
					if tc.trustErr {
						return github.TrustDecision{}, errors.New("unreadable history")
					}
					return github.TrustDecision{Reason: "untrusted actor"}, nil
				}
			}
			global := 2
			if tc.name == "global capacity" {
				global = 1
			}
			yaml := "concurrency: " + strconv.Itoa(global) + "\nrepositories:\n  - repo: owner/repo\n" + tc.yaml
			s, rt := testkit.Configured(t, api, yaml, testkit.RejectingGit{})
			if tc.active {
				if tc.name != "active" {
					other := testkit.Ready(8)
					other.Labels = nil
					api.Issues["owner/repo"] = append(api.Issues["owner/repo"], other)
				}
				number := 8
				if tc.name == "active" {
					number = 7
				}
				if _, err := rt.Workflow.Transition(ctx, "existing", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: number}); err != nil {
					t.Fatal(err)
				}
				if _, err := rt.Workflow.Transition(ctx, "existing", workflow.Request{Trigger: workflow.OperationFailed, Failure: &fault.Error{Code: "test.failure", Message: "fixture"}}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.limited {
				testkit.LimitedHarness(t, rt, "claude", time.Now().Add(time.Hour))
			}
			if tc.paused {
				if err := s.Pause(ctx, true); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := rt.Events.History(ctx, 0, 100)
			original, _ := json.Marshal(api.Issues)
			if _, err := rt.DB.Exec("PRAGMA query_only=1"); err != nil {
				t.Fatal(err)
			}
			report, err := s.Plan(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Issues) != 1 || report.Issues[0].Decision != tc.decision || report.Issues[0].Reason != tc.reason {
				t.Fatalf("plan=%+v want %s:%s", report, tc.decision, tc.reason)
			}
			cfg, _, err := config.Parse([]byte(yaml))
			if err != nil {
				t.Fatal(err)
			}
			reader, err := store.OpenReadOnly(ctx, rt.Workspace.DatabasePath())
			if err != nil {
				t.Fatal(err)
			}
			offline, err := scheduler.PlanOffline(ctx, cfg, reader, scheduler.Dependencies{GitHub: api}, "")
			reader.Close()
			// Pause is an in-memory control-plane gate, absent offline.
			if !tc.paused && (err != nil || !reflect.DeepEqual(report, offline)) {
				t.Fatalf("offline=%+v online=%+v err=%v", offline, report, err)
			}

			if tc.name == "blocked" && !reflect.DeepEqual(report.Issues[0].Blockers, []int{42, 99}) {
				t.Fatalf("blockers=%v", report.Issues[0].Blockers)
			}
			after, _ := rt.Events.History(ctx, 0, 100)
			observed, _ := json.Marshal(api.Issues)
			if string(original) != string(observed) || len(api.TrustComments) != 0 || len(api.OverrideComments) != 0 || !reflect.DeepEqual(before, after) {
				t.Fatal("plan mutated GitHub or store")
			}
			if _, err := rt.DB.Exec("PRAGMA query_only=0"); err != nil {
				t.Fatal(err)
			}
			if err := s.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			runs, err := s.Runs(ctx)
			if err != nil {
				t.Fatal(err)
			}
			claimed := false
			for _, run := range runs {
				if run.ID != "existing" && run.IssueNumber == 7 {
					claimed = true
				}
			}
			if claimed != (tc.decision == "would_claim") {
				t.Fatalf("next poll claimed=%t plan=%+v", claimed, report)
			}
		})
	}
}

func TestPlanReservesSlotsInSchedulingOrderAndProjectsRepository(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	older := testkit.Ready(3)
	older.CreatedAt = older.CreatedAt.Add(-time.Hour)
	unready := testkit.Ready(4)
	unready.Labels = nil
	closed := testkit.Ready(5)
	closed.State = github.Closed
	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/first": {testkit.Ready(2), closed, testkit.Ready(1), older, unready}, "owner/second": {testkit.Ready(10)}}}
	s, _ := testkit.Configured(t, api, "concurrency: 2\nrepositories:\n  - repo: owner/first\n    concurrency: 1\n  - repo: owner/second\n", testkit.RejectingGit{})
	plan, err := s.Plan(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	numbers := []int{}
	decisions := []string{}
	for _, issue := range plan.Issues {
		numbers = append(numbers, issue.Number)
		decisions = append(decisions, issue.Decision)
	}
	if !reflect.DeepEqual(numbers, []int{3, 1, 2, 10}) || !reflect.DeepEqual(decisions, []string{"would_claim", "waiting", "waiting", "would_claim"}) {
		t.Fatalf("plan=%+v", plan)
	}
	filtered, err := s.Plan(ctx, "OWNER/SECOND")
	if err != nil || len(filtered.Issues) != 1 || !reflect.DeepEqual(filtered.Issues[0], plan.Issues[3]) {
		t.Fatalf("filter=%+v err=%v", filtered, err)
	}
	if _, err := s.Plan(ctx, "owner/missing"); err == nil {
		t.Fatal("unknown repository accepted")
	}
	if api.Issues["owner/first"][0].Number != 2 {
		t.Fatal("adapter-owned issue order changed")
	}
}

func TestPlanUsesResolvedRoleSettingsAndImplementerGate(t *testing.T) {
	t.Parallel()

	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}}
	api.Issues["owner/repo"][0].Labels = append(api.Issues["owner/repo"][0].Labels, github.Label{Name: "mergeyard:implementer=codex"})
	s, rt := testkit.Configured(t, api, "repositories:\n  - repo: owner/repo\n    implementer:\n      agent: claude\n      model: original\n      effort: high\n      skills: [implement]\n    reviewer:\n      model: review-model\n      effort: medium\n      skills: [review]\n", testkit.RejectingGit{})
	testkit.LimitedHarness(t, rt, "claude", time.Now().Add(time.Hour))
	plan, err := s.Plan(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	want := &config.RunRoleSettings{Implementer: config.RoleSettings{Agent: "codex", Skills: []string{"implement"}, HarnessSource: "label"}, Reviewer: config.RoleSettings{Agent: "claude", Model: "review-model", Effort: "medium", Skills: []string{"review"}, HarnessSource: "config"}}
	if len(plan.Issues) != 1 || plan.Issues[0].Decision != "would_claim" || !reflect.DeepEqual(plan.Issues[0].RoleSettings, want) {
		t.Fatalf("plan=%+v", plan)
	}
}

func TestPlanHTTPReadsOnly(t *testing.T) {
	t.Parallel()

	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}}
	s, rt := testkit.Configured(t, api, "repositories: [{repo: owner/repo}]", testkit.RejectingGit{})
	server, err := web.NewWithOperations(rt.Events, rt.Scheduler, s, rt.Workspace.Root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.DB.Exec("PRAGMA query_only=1"); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, testkit.ApiRequest(t, rt.Workspace.Root, http.MethodGet, "http://127.0.0.1:7331/api/plan?repo=owner%2Frepo"))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"decision":"would_claim"`) {
		t.Fatalf("HTTP plan=%d %s", response.Code, response.Body)
	}
	history, err := rt.Events.History(context.Background(), 0, 100)
	if err != nil || len(history) != 0 || len(api.TrustComments) != 0 || len(api.OverrideComments) != 0 {
		t.Fatalf("plan wrote history=%v err=%v", history, err)
	}
}

func TestPlanRepositoryFilterRetainsEarlierGlobalReservations(t *testing.T) {
	t.Parallel()

	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/first": {testkit.Ready(1)}, "owner/second": {testkit.Ready(2)}}}
	s, _ := testkit.Configured(t, api, "concurrency: 1\nrepositories: [{repo: owner/first}, {repo: owner/second}]", testkit.RejectingGit{})
	report, err := s.Plan(context.Background(), "owner/second")
	if err != nil || len(report.Issues) != 1 || report.Issues[0].Decision != "waiting" || report.Issues[0].Reason != "concurrency_full" {
		t.Fatalf("filter=%+v err=%v", report, err)
	}
}

func TestPlanExpiredHarnessLimitAndEmptyJSON(t *testing.T) {
	t.Parallel()

	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}}
	s, rt := testkit.Configured(t, api, "repositories: [{repo: owner/repo}]", testkit.RejectingGit{})
	_, err := rt.DB.Exec("INSERT INTO harness_limits(harness_type,limited_until,reset_time_source,signal_source) VALUES('claude',?,'reported','fixture')", time.Now().Add(-time.Hour).Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.Plan(context.Background(), "")
	if err != nil || len(report.Issues) != 1 || report.Issues[0].Decision != "would_claim" {
		t.Fatalf("expired gate=%+v err=%v", report, err)
	}
	api.Issues["owner/repo"] = nil
	report, err = s.Plan(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(report)
	if err != nil || string(data) != `{"schema_version":1,"paused":false,"issues":[]}` {
		t.Fatalf("empty plan=%s err=%v", data, err)
	}
}
