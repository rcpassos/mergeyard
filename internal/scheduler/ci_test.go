package scheduler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestCINoChecksWindowAndRestartWhilePaused(t *testing.T) {
	t.Parallel()

	flow := testkit.NewScenario(t)
	runtime, base, cfg, r := flow.Runtime, flow.Api, flow.Cfg, flow.Runner
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	api := &testkit.CiGitHub{FakeGitHub: base, ApplyReady: true}
	deps := scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }}
	s, err := flow.New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	run := flow.Until(s, workflow.WaitingForCI, workflow.Review)
	if run.CI == nil || !run.CI.StartedAt.Equal(now) || !run.CI.Deadline.Equal(now.Add(time.Hour)) {
		t.Fatalf("wait not started with approval: %+v", run.CI)
	}
	s.Pause(context.Background(), true)
	now = now.Add(2*time.Minute - time.Nanosecond)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	run, _ = runtime.Workflow.Get(context.Background(), run.ID)
	if run.State != workflow.WaitingForCI || api.ReadyCalls != 0 {
		t.Fatal("premature readiness")
	}
	// Reopening the scheduler must retain the original wait, even with new config.
	cfg.CITimeout = time.Minute
	s, err = flow.New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Nanosecond)
	s.Pause(context.Background(), true)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	run, _ = runtime.Workflow.Get(context.Background(), run.ID)
	if run.State != workflow.ReadyToMerge || api.ReadyCalls != 1 || !run.CI.Deadline.Equal(run.CI.StartedAt.Add(time.Hour)) {
		t.Fatalf("readiness: %+v %+v calls %d", run, run.CI, api.ReadyCalls)
	}
	if api.Prs["mergeyard/issue-7"].State != github.Open {
		t.Fatal("merged PR")
	}
}

func TestCICheckMatrix(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		Checks   []ci.Check
		required []ci.Requirement
		allow    bool
		state    workflow.State
		Code     string
	}{
		{name: "required success", Checks: []ci.Check{{Name: "build", Source: "check", AppID: 42, Status: "completed", Conclusion: "success"}}, required: []ci.Requirement{{Name: "build", AppID: 42}}, state: workflow.ReadyToMerge},
		{name: "optional pending blocks", Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "success"}, {Name: "optional", Source: "check", Status: "in_progress"}}, state: workflow.WaitingForCI},
		{name: "missing required blocks", required: []ci.Requirement{{Name: "build"}}, state: workflow.WaitingForCI},
		{name: "wrong app blocks", Checks: []ci.Check{{Name: "build", Source: "check", AppID: 1, Status: "completed", Conclusion: "success"}}, required: []ci.Requirement{{Name: "build", AppID: 42}}, state: workflow.WaitingForCI},
		{name: "allowed skipped and neutral", allow: true, Checks: []ci.Check{{Name: "skip", Source: "check", Status: "completed", Conclusion: "skipped"}, {Name: "neutral", Source: "check", Status: "completed", Conclusion: "neutral"}}, state: workflow.ReadyToMerge},
		{name: "disallowed skipped", Checks: []ci.Check{{Name: "skip", Source: "check", Status: "completed", Conclusion: "skipped"}}, state: workflow.WaitingForCI},
		{name: "disallowed neutral", Checks: []ci.Check{{Name: "neutral", Source: "check", Status: "completed", Conclusion: "neutral"}}, state: workflow.WaitingForCI},
		{name: "unknown conclusion", Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "future"}}, state: workflow.WaitingForCI},
		{name: "stale", Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "stale"}}, state: workflow.WaitingForCI},
		{name: "wrong check SHA", Checks: []ci.Check{{Name: "build", SHA: "old", Source: "check", Status: "completed", Conclusion: "success"}}, state: workflow.WaitingForCI},
		{name: "optional failure", Checks: []ci.Check{{Name: "optional", Source: "check", Status: "completed", Conclusion: "failure"}}, state: workflow.Active},
		{name: "timed out", Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "timed_out"}}, state: workflow.Active},
		{name: "cancelled", Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "cancelled"}}, state: workflow.NeedsAttention, Code: "ci.action_required"},
		{name: "action required", Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "action_required"}}, state: workflow.NeedsAttention, Code: "ci.action_required"},
		{name: "legacy failure", Checks: []ci.Check{{Name: "legacy", Source: "status", Conclusion: "failure"}}, state: workflow.Active},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			flow := testkit.NewScenario(t)
			runtime, base, cfg, r := flow.Runtime, flow.Api, flow.Cfg, flow.Runner
			now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
			api := &testkit.CiGitHub{FakeGitHub: base, Evidence: ci.Evidence{Checks: tc.Checks, Required: tc.required, AllowSkippedNeutral: tc.allow}, ApplyReady: true}
			s, err := flow.New(cfg, scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			run := flow.Until(s, workflow.WaitingForCI, workflow.Review)
			now = now.Add(3 * time.Minute)
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			got, err := runtime.Workflow.Get(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != tc.state || got.LastErrorCode != tc.Code || (tc.state != workflow.ReadyToMerge && api.ReadyCalls != 0) {
				t.Fatalf("state=%s error=%s writes=%d", got.State, got.LastErrorCode, api.ReadyCalls)
			}
			if got.CI == nil || len(got.CI.Evidence.Checks) != len(tc.Checks) {
				t.Fatal("check evidence not retained")
			}
			var attempts int
			if err := runtime.DB.QueryRow("SELECT count(*) FROM phase_attempts WHERE run_id=?", run.ID).Scan(&attempts); err != nil || attempts != 2 {
				t.Fatalf("CI launched an agent: %d %v", attempts, err)
			}
		})
	}
}

func TestCITimeoutRetainsDeadlineAcrossUnknownAndRestart(t *testing.T) {
	t.Parallel()

	flow := testkit.NewScenario(t)
	runtime, base, cfg, r := flow.Runtime, flow.Api, flow.Cfg, flow.Runner
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	api := &testkit.CiGitHub{FakeGitHub: base, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "queued"}}}}
	deps := scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }}
	s, err := flow.New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	run := flow.Until(s, workflow.WaitingForCI, workflow.Review)
	now = now.Add(30 * time.Minute)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	api.QueryError = fmt.Errorf("requirements permission denied")
	s, err = flow.New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(30*time.Minute - time.Nanosecond)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, _ := runtime.Workflow.Get(context.Background(), run.ID)
	if saved.State != workflow.WaitingForCI || saved.CI.QueryError == "" || !saved.CI.Deadline.Equal(run.CI.Deadline) {
		t.Fatalf("renewed or lost wait: %+v", saved.CI)
	}
	now = now.Add(time.Nanosecond)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, _ = runtime.Workflow.Get(context.Background(), run.ID)
	if saved.State != workflow.NeedsAttention || saved.LastErrorCode != "ci.wait_timeout" || api.ReadyCalls != 0 {
		t.Fatalf("timeout: %+v", saved)
	}
}

func TestCIHeadRacesAndReadinessAmbiguity(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"before checks", "during checks", "after mutation", "write applied with error", "write not applied", "already normal", "commit acknowledgement failed", "query crosses deadline"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			flow := testkit.NewScenario(t)
			runtime, base, cfg, r := flow.Runtime, flow.Api, flow.Cfg, flow.Runner
			now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
			api := &testkit.CiGitHub{FakeGitHub: base, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "success"}}}, ApplyReady: true}
			deps := scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }}
			s, err := flow.New(cfg, deps)
			if err != nil {
				t.Fatal(err)
			}
			run := flow.Until(s, workflow.WaitingForCI, workflow.Review)
			pr := base.Prs["mergeyard/issue-7"]
			switch mode {
			case "before checks":
				pr.Head.SHA = "replacement"
			case "during checks":
				api.AfterEvidence = func() { pr.Head.SHA = "replacement" }
			case "after mutation":
				api.AfterReady = func() { pr.Head.SHA = "replacement" }
			case "write applied with error":
				api.ReadyError = fmt.Errorf("connection lost")
			case "write not applied":
				api.ReadyError = fmt.Errorf("permission denied")
				api.ApplyReady = false
			case "already normal":
				pr.Draft = false
			case "query crosses deadline":
				api.AfterEvidence = func() { now = now.Add(time.Hour) }
			case "commit acknowledgement failed":
				if _, err := runtime.DB.Exec(`CREATE TRIGGER reject_ready BEFORE INSERT ON events WHEN NEW.type='ci.updated' AND NEW.payload_json LIKE '%ci_passed%' BEGIN SELECT RAISE(FAIL,'crash'); END`); err != nil {
					t.Fatal(err)
				}
			}
			err = s.Tick(context.Background())
			if mode == "commit acknowledgement failed" {
				if err == nil {
					t.Fatal("ready acknowledgement should fail")
				}
				saved, _ := runtime.Workflow.Get(context.Background(), run.ID)
				if saved.State != workflow.WaitingForCI || !saved.CI.ReadyStarted {
					t.Fatal("readiness intent lost")
				}
				runtime.DB.Exec("DROP TRIGGER reject_ready")
				s, err = flow.New(cfg, deps)
				if err != nil {
					t.Fatal(err)
				}
				err = s.Tick(context.Background())
			}
			if err != nil {
				t.Fatal(err)
			}
			saved, _ := runtime.Workflow.Get(context.Background(), run.ID)
			switch mode {
			case "before checks", "during checks", "after mutation":
				if saved.State != workflow.NeedsAttention || saved.ApprovedSHA != "" || saved.LastErrorCode != "ci.head_changed" {
					t.Fatalf("stale readiness: %+v", saved)
				}
			case "query crosses deadline":
				if saved.State != workflow.NeedsAttention || saved.LastErrorCode != "ci.wait_timeout" || api.ReadyCalls != 0 {
					t.Fatalf("deadline crossed: %+v", saved)
				}
			case "write not applied":
				if saved.State != workflow.NeedsAttention || !saved.CI.ReadyStarted {
					t.Fatalf("ambiguity lost: %+v", saved)
				}
			default:
				if saved.State != workflow.ReadyToMerge {
					t.Fatalf("readiness: %+v", saved)
				}
			}
			calls := api.ReadyCalls
			for range 2 {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if api.ReadyCalls != calls || (mode == "already normal" && calls != 0) {
				t.Fatal("readiness write replayed")
			}
		})
	}
}

func TestReadyWarningsClosureCapacityAndHTTP(t *testing.T) {
	t.Parallel()

	flow := testkit.NewScenario(t)
	runtime, base, cfg, r := flow.Runtime, flow.Api, flow.Cfg, flow.Runner
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	api := &testkit.CiGitHub{FakeGitHub: base, Evidence: ci.Evidence{AllowSkippedNeutral: true, Checks: []ci.Check{{Name: "<script>check</script>", Source: "check", Status: "completed", Conclusion: "neutral", URL: "javascript:alert(1)"}}}, ApplyReady: true}
	s, err := flow.New(cfg, scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	run := flow.Until(s, workflow.WaitingForCI, workflow.Review)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, _ := runtime.Workflow.Get(context.Background(), run.ID)
	if saved.State != workflow.ReadyToMerge {
		t.Fatal("not ready")
	}
	approved := saved.ApprovedSHA
	api.Prs["mergeyard/issue-7"].Head.SHA = "later-commit"
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, _ = runtime.Workflow.Get(context.Background(), run.ID)
	if saved.State != workflow.ReadyToMerge || saved.ApprovedSHA != approved || !strings.Contains(saved.CI.Warning, "changed-after-approval") {
		t.Fatalf("post-readiness warning: %+v", saved)
	}
	server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/runs/" + run.ID, "/"} {
		res := httptest.NewRecorder()
		server.ServeHTTP(res, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331"+path, nil))
		body := res.Body.String()
		if res.Code != 200 || !strings.Contains(body, "Ready to merge") {
			t.Fatalf("HTTP %d: %s", res.Code, body)
		}
		if path != "/" {
			for _, text := range []string{"Wait started", "Wait deadline", "neutral", "passed", "&lt;script&gt;check&lt;/script&gt;", "later-commit", "changed-after-approval"} {
				if !strings.Contains(body, text) {
					t.Fatalf("missing %q", text)
				}
			}
			if strings.Contains(body, `href="javascript:alert`) || strings.Contains(body, "<script>check") {
				t.Fatal("unescaped check content")
			}
		} else if !strings.Contains(body, "0 / 1") {
			t.Fatal("dashboard still consumes slot")
		}
	}
	res := httptest.NewRecorder()
	server.ServeHTTP(res, testkit.ApiRequest(t, runtime.Workspace.Root, http.MethodGet, "http://127.0.0.1:7331/api/status"))
	var status web.Status
	if err := json.Unmarshal(res.Body.Bytes(), &status); err != nil || len(status.Runs) != 1 || status.Runs[0].CI.Warning == "" {
		t.Fatal("API lost CI evidence")
	}
	res = httptest.NewRecorder()
	server.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/static/app.js", nil))
	if !strings.Contains(res.Body.String(), "ci.updated") || !strings.Contains(res.Body.String(), "pr.readiness_started") {
		t.Fatal("live page lacks CI events")
	}
	// A ready PR frees the one slot for unrelated work.
	api.Issues["owner/repo"] = append(api.Issues["owner/repo"], testkit.Ready(8))
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(context.Background())
	if err != nil || len(runs) != 2 {
		t.Fatalf("capacity not released: %+v %v", runs, err)
	}
	api.Prs["mergeyard/issue-7"].State = github.Closed
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, _ = runtime.Workflow.Get(context.Background(), run.ID)
	if saved.State != workflow.NeedsAttention || saved.LastErrorCode != "ci.pr_closed" {
		t.Fatalf("closure: %+v", saved)
	}
}

func TestTestMergeEvidenceRacesPreserveApprovalAndDeadline(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"test merge changed during query", "base changed during query", "merge generation pending", "stale merge evidence", "test merge changed during readiness"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			flow := testkit.NewScenario(t)
			runtime, base, cfg, r := flow.Runtime, flow.Api, flow.Cfg, flow.Runner
			now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
			api := &testkit.CiGitHub{FakeGitHub: base, ApplyReady: true, Evidence: ci.Evidence{MergeSHA: "merge-1", Checks: []ci.Check{{Name: "head", Source: "check", Status: "completed", Conclusion: "success"}, {Name: "merge", SHA: "merge-1", Source: "check", Status: "completed", Conclusion: "success"}}}}
			s, err := flow.New(cfg, scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			run := flow.Until(s, workflow.WaitingForCI, workflow.Review)
			pr := api.Prs["mergeyard/issue-7"]
			yes := true
			pr.Mergeable = &yes
			pr.MergeCommitSHA = "merge-1"
			pr.Base.SHA = fmt.Sprintf("%040x", 1)
			switch mode {
			case "test merge changed during query":
				api.AfterEvidence = func() { pr.MergeCommitSHA = "merge-2" }
			case "base changed during query":
				api.AfterEvidence = func() { pr.Base.SHA = "base-2" }
			case "merge generation pending":
				api.AfterEvidence = func() { pr.Mergeable = nil }
			case "stale merge evidence":
				api.Evidence.MergeSHA = "old-merge"
				api.Evidence.Checks[1].SHA = "old-merge"
			case "test merge changed during readiness":
				api.AfterReady = func() { pr.MergeCommitSHA = "merge-2" }
			}
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			saved, err := runtime.Workflow.Get(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			expected := workflow.WaitingForCI
			calls := 0
			if mode == "test merge changed during readiness" {
				expected = workflow.NeedsAttention
				calls = 1
			}
			if saved.State != expected || saved.ApprovedSHA != run.ApprovedSHA || !saved.CI.Deadline.Equal(run.CI.Deadline) || api.ReadyCalls != calls || (expected == workflow.WaitingForCI && saved.CI.QueryError == "") {
				t.Fatalf("stale merge readiness: %+v CI=%+v calls=%d", saved, saved.CI, api.ReadyCalls)
			}
		})
	}
}

func TestCIReadinessAfterMergedImplementerFlows(t *testing.T) {

	for _, mode := range []string{"Claude fix and re-review", "Codex implementation"} {
		t.Run(mode, func(t *testing.T) {
			var flow *testkit.Scenario
			if mode == "Codex implementation" {
				flow = testkit.NewScenario(t)
				flow.Cfg.Repositories[0].Implementer.Agent = "codex"
			} else {
				flow = testkit.NewPairingScenario(t, "claude", "claude", testkit.FixedReport)
			}
			_, runtime, base, _, cfg, r := flow.Parts()
			now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
			api := &testkit.CiGitHub{FakeGitHub: base, ApplyReady: true, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "success"}}}}
			s, err := flow.New(cfg, scheduler.Dependencies{GitHub: testkit.CiBranchGitHub{CiGitHub: api}, Runner: r, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			run := flow.Until(s, workflow.WaitingForCI, workflow.Review)
			if run.CI == nil || run.CI.SHA != run.ApprovedSHA || !run.CI.StartedAt.Equal(now) {
				t.Fatal("approval did not start a pinned CI wait")
			}
			if mode == "Claude fix and re-review" {
				if run.ReviewRound != 2 || run.Fix == nil || !run.Fix.Pushed {
					t.Fatal("fix history lost")
				}
			} else if run.Implementer == nil || run.Implementer.Agent != "codex" || run.Implementer.SessionID != testkit.CodexID {
				t.Fatal("Codex session lost")
			}
			if _, err := s.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			saved, err := runtime.Workflow.Get(context.Background(), run.ID)
			if err != nil || saved.State != workflow.ReadyToMerge || api.ReadyCalls != 1 || saved.CI.CurrentHead != saved.ApprovedSHA {
				t.Fatalf("merged flow readiness: %+v %v", saved, err)
			}
		})
	}
}
