package scheduler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestObservedMergeCompletesCoding(t *testing.T) {
	t.Parallel()

	flow := testkit.NewScenario(t)
	s, runtime, api, _, _, _ := flow.Parts()
	run := flow.Until(s, workflow.WaitingForCI, workflow.Review)
	pr := api.Prs["mergeyard/issue-7"]
	pr.State, pr.Merged = github.Closed, true
	api.Issues["owner/repo"][0].State = github.Closed
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, err := runtime.Workflow.Get(context.Background(), run.ID)
	if err != nil || saved.State != workflow.Completed {
		t.Fatalf("merged coding run = %+v, %v", saved, err)
	}
}

func TestMergeCleanupPendingIsVisible(t *testing.T) {
	t.Parallel()

	flow := testkit.NewScenario(t)
	s, runtime, api, _, cfg, _ := flow.Parts()
	run := flow.Until(s, workflow.WaitingForCI, workflow.Review)
	api.Prs["mergeyard/issue-7"].State, api.Prs["mergeyard/issue-7"].Merged = github.Closed, true
	api.Mutate = func(string, string, int, string) error { return fmt.Errorf("GitHub unavailable <script>") }
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, _ := runtime.Workflow.Get(context.Background(), run.ID)
	if saved.State != workflow.Completed || saved.Merge == nil || !saved.Merge.Pending() {
		t.Fatalf("pending merge: %+v", saved)
	}
	server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/runs/" + run.ID} {
		res := httptest.NewRecorder()
		server.ServeHTTP(res, testkit.DashboardRequest(t, server, "GET", "http://127.0.0.1:7331"+path, nil))
		if res.Code != 200 || !strings.Contains(strings.ToLower(res.Body.String()), "cleanup pending") {
			t.Fatalf("merge missing from %s", path)
		}
		if path != "/" && (!strings.Contains(res.Body.String(), "Remove running label") || strings.Contains(res.Body.String(), "unavailable <script>")) {
			t.Fatal("missing or unsafe maintenance details")
		}
	}
	res := httptest.NewRecorder()
	server.ServeHTTP(res, testkit.ApiRequest(t, runtime.Workspace.Root, "GET", "http://127.0.0.1:7331/api/status"))
	var status web.Status
	if err := json.Unmarshal(res.Body.Bytes(), &status); err != nil || len(status.Runs) != 1 || status.Runs[0].Merge == nil || !status.Runs[0].Merge.Pending() {
		t.Fatal("status lost maintenance")
	}
}

func TestAmbiguousMergeCleanupDoesNotRedispatchSourceIssue(t *testing.T) {
	t.Parallel()

	for _, applied := range []bool{false, true} {
		t.Run(fmt.Sprint(applied), func(t *testing.T) {
			t.Parallel()

			flow := testkit.NewScenario(t)
			_, runtime, base, _, cfg, r := flow.Parts()
			api := &testkit.MergeGitHub{CiGitHub: &testkit.CiGitHub{FakeGitHub: base}, ApplyClose: applied, CloseError: fmt.Errorf("lost close response")}
			s, err := flow.New(cfg, scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			run := flow.Until(s, workflow.WaitingForCI, workflow.Review)
			base.Prs["mergeyard/issue-7"].State, base.Prs["mergeyard/issue-7"].Merged = github.Closed, true
			// The ready removal fails while other label removals succeed: pending
			// maintenance must still exclude this source issue from dispatch.
			base.Issues["owner/repo"][0].Labels = append(base.Issues["owner/repo"][0].Labels, github.Label{Name: "ready-for-agent"})
			base.Mutate = func(action, repo string, n int, label string) error {
				if n == 7 && label == "ready-for-agent" {
					return fmt.Errorf("remove unavailable")
				}
				return nil
			}
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			saved, _ := runtime.Workflow.Get(context.Background(), run.ID)
			if saved.State != workflow.Completed || !saved.Merge.Pending() {
				t.Fatal("ambiguous closure lost")
			}
			base.Issues["owner/repo"] = append(base.Issues["owner/repo"], testkit.Ready(8))
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			runs, _ := s.Runs(context.Background())
			if len(runs) != 2 {
				t.Fatalf("source redispatched or cleanup held capacity: %+v", runs)
			}
			for _, v := range runs {
				if v.ID != run.ID && v.IssueNumber != 8 {
					t.Fatal("redispatched merged issue")
				}
			}
			calls := api.CloseCalls
			if applied && calls != 1 {
				t.Fatal("applied closure was replayed")
			}
			base.Mutate = nil
			api.ApplyClose = true
			api.CloseError = nil
			for range 2 {
				if _, err := s.Reconcile(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			saved, _ = runtime.Workflow.Get(context.Background(), run.ID)
			if saved.Merge.Pending() || saved.Merge.Error != "" {
				t.Fatal("closure not recovered")
			}
		})
	}
}

func TestMergedAttentionAndManualRunsCompleteButStoppedRunsStayStopped(t *testing.T) {
	t.Parallel()

	for _, state := range []workflow.State{workflow.NeedsAttention, workflow.Manual, workflow.Stopped} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()

			flow := testkit.NewScenario(t)
			s, runtime, api, _, _, _ := flow.Parts()
			run := flow.Until(s, workflow.WaitingForCI, workflow.Review)
			trigger := workflow.OperationFailed
			if state == workflow.Manual {
				trigger = workflow.TakeOver
			}
			if state == workflow.Stopped {
				if err := s.Stop(context.Background(), run.ID); err != nil {
					t.Fatal(err)
				}
			} else if _, err := runtime.Workflow.Transition(context.Background(), run.ID, workflow.Request{Trigger: trigger, Failure: &fault.Error{Code: "test.attention", Message: "Inspect work"}}); err != nil {
				t.Fatal(err)
			}
			api.Prs["mergeyard/issue-7"].State, api.Prs["mergeyard/issue-7"].Merged = github.Closed, true
			api.Issues["owner/repo"][0].State = github.Closed
			if _, err := s.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			saved, _ := runtime.Workflow.Get(context.Background(), run.ID)
			if state == workflow.Stopped {
				if saved.State != workflow.Stopped || saved.Merge != nil {
					t.Fatal("stopped run reactivated")
				}
			} else if saved.State != workflow.Completed || saved.Merge == nil || !saved.Merge.Early {
				t.Fatalf("merge not authoritative: %+v", saved)
			}
		})
	}
}

func TestReadyMergeAndCompletedPublicationRemainIndependent(t *testing.T) {
	t.Parallel()

	flow := testkit.NewScenario(t)
	_, runtime, base, _, cfg, r := flow.Parts()
	// Queue the report without delivering it, so its first attempt happens
	// after completion. A completed run still gets that never-attempted chance.
	cfg.PRComments = false
	publisher := &testkit.ReportGitHub{BranchGitHub: testkit.BranchGitHub{FakeGitHub: base}}
	api := &testkit.MergedPublisher{MergeGitHub: &testkit.MergeGitHub{CiGitHub: &testkit.CiGitHub{FakeGitHub: base, ApplyReady: true, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "success"}}}}, ApplyClose: true}, Publisher: publisher}
	s, err := flow.New(cfg, scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := flow.Until(s, workflow.ReadyToMerge, workflow.Review)
	base.Prs["mergeyard/issue-7"].State, base.Prs["mergeyard/issue-7"].Merged = github.Closed, true
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, _ := runtime.Workflow.Get(context.Background(), run.ID)
	if saved.State != workflow.Completed || saved.Merge.Early || saved.Merge.Pending() || len(saved.Publications) != 1 || saved.Publications[0].State != "pending" {
		t.Fatalf("ready merge: %+v %+v", saved, saved.Merge)
	}
	cfg.PRComments = true
	s, err = flow.New(cfg, scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	saved, _ = runtime.Workflow.Get(context.Background(), run.ID)
	if len(publisher.Bodies) != 1 || saved.Publications[0].State != "published" || saved.State != workflow.Completed {
		t.Fatal("cleanup blocked completed publication")
	}
}

func TestMergeObservedDuringCIRepairEvidenceCompletesWithoutFix(t *testing.T) {
	t.Parallel()

	flow := testkit.NewScenario(t)
	_, runtime, base, _, cfg, r := flow.Parts()
	api := &testkit.MergeGitHub{CiGitHub: &testkit.CiGitHub{FakeGitHub: base, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "failure"}}}}, ApplyClose: true}
	s, err := flow.New(cfg, scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := flow.Until(s, workflow.WaitingForCI, workflow.Review)
	api.AfterEvidence = func() {
		base.Prs["mergeyard/issue-7"].State, base.Prs["mergeyard/issue-7"].Merged = github.Closed, true
		base.Issues["owner/repo"][0].State = github.Closed
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, err := runtime.Workflow.Get(context.Background(), run.ID)
	if err != nil || saved.State != workflow.Completed || saved.Merge == nil || !saved.Merge.Early {
		t.Fatalf("merge lost during repair check: %+v %v", saved, err)
	}
	if len(saved.FixHistory) != 0 {
		t.Fatal("merge launched a CI repair")
	}
}

func TestFailedPRReadDoesNotBypassExpiredCIWait(t *testing.T) {
	t.Parallel()

	flow := testkit.NewScenario(t)
	_, runtime, base, _, cfg, r := flow.Parts()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	api := &testkit.FailedPRReadGitHub{MergeGitHub: &testkit.MergeGitHub{CiGitHub: &testkit.CiGitHub{FakeGitHub: base}, ApplyClose: true}}
	s, err := flow.New(cfg, scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	run := flow.Until(s, workflow.WaitingForCI, workflow.Review)
	now = run.CI.Deadline
	api.ReadError = fmt.Errorf("PR read unavailable")
	_, reconcileErr := s.Reconcile(context.Background())
	saved, err := runtime.Workflow.Get(context.Background(), run.ID)
	if err != nil || saved.State != workflow.NeedsAttention || saved.LastErrorCode != "ci.wait_timeout" || saved.CI.QueryError == "" {
		t.Fatalf("failed PR read bypassed deadline: state=%s code=%s reconcile=%v get=%v", saved.State, saved.LastErrorCode, reconcileErr, err)
	}
}
