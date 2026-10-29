package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestCreditRetrySavedImplementationDoesNotReserveProbe(t *testing.T) {
	t.Parallel()

	_, rt, base, _, cfg, r := testkit.TmuxFlow(t, `case "$(git branch --show-current)" in mergeyard/issue-7) `+testkit.SuccessfulScript+`;; *) exit 1;; esac`)
	cfg.Concurrency = 2
	cfg.Repositories[0].Concurrency = 2
	base.Issues["owner/repo"] = append(base.Issues["owner/repo"], testkit.Ready(8))
	api := &testkit.RetryPublicationGitHub{FakeGitHub: base, Unavailable: true}
	h := &testkit.CreditHarness{ClassifiedHarness: &testkit.ClassifiedHarness{HarnessAdapter: harness.NewClaude(cfg.Agents.Claude), Outcome: harness.FailureClassification{Kind: harness.CreditsExhausted}}, Detection: true}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{"claude": h}})
	if err != nil {
		t.Fatal(err)
	}
	var saved workflow.Run
	testkit.WaitFor(t, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != 2 {
			return false
		}
		for _, run := range runs {
			if run.IssueNumber == 7 {
				saved = run
			}
		}
		return runs[0].State == workflow.NeedsAttention && runs[1].State == workflow.NeedsAttention
	})
	if saved.Implementer.Status != "succeeded" {
		t.Fatalf("saved implementation fixture: %+v", saved)
	}
	api.Unavailable = false
	selected, err := s.Retry(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.CreditProbes) != 0 {
		t.Fatalf("publication-only Retry reserved a credit probe: %+v", selected.CreditProbes)
	}
	testkit.WaitFor(t, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		got, err := rt.Workflow.Get(context.Background(), saved.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.State == workflow.WaitingForHarness && got.Phase == workflow.Review
	})
	states, err := s.HarnessAvailability(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range states {
		if state.Harness == "claude" && (state.Available || state.ProbeID != "") {
			t.Fatalf("publication affected credit block: %+v", state)
		}
	}
}

func TestCreditReviewDiscoveryFailureReleasesProbe(t *testing.T) {
	t.Parallel()

	_, rt, api, _, cfg, r := testkit.TmuxFlow(t, testkit.ReviewScript(`exit 1`))
	cfg.Repositories[0].Reviewer.MaxAttempts = 2
	h := &testkit.DiscoveryFailureCreditHarness{CreditHarness: &testkit.CreditHarness{ClassifiedHarness: &testkit.ClassifiedHarness{HarnessAdapter: harness.NewClaude(cfg.Agents.Claude), Outcome: harness.FailureClassification{Kind: harness.CreditsExhausted}}, Detection: true}}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{"claude": h}})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Review)
	h.Reject = true
	testkit.ReplacePhaseScript(t, cfg, workflow.Review, testkit.ReviewScript(`printf 'probe-ready\n'; sleep 30; exit 1`))
	if _, err := s.Retry(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	testkit.WaitFor(t, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		run, err = rt.Workflow.Get(context.Background(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		return run.Review.Attempt == 2 && run.Review.Status == "failed"
	})
	if len(run.CreditProbes) != 1 || run.CreditProbes[0].Status != "released" {
		t.Fatalf("ended discovery failure still owns probe: %+v", run.CreditProbes)
	}
	states, err := s.HarnessAvailability(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range states {
		if state.Harness == "claude" && (state.Available || state.ProbeID != "") {
			t.Fatalf("discovery failure changed credit availability: %+v", state)
		}
	}
}

func TestCreditRetrySavedFixDoesNotReserveProbe(t *testing.T) {
	t.Parallel()

	_, rt, api, _, cfg, r := testkit.TmuxFlow(t, testkit.LoopScript(`case "$(git branch --show-current)" in mergeyard/issue-8) exit 1;; esac
printf fixed > feature.txt; `+testkit.FixedReport))
	cfg.Concurrency = 2
	cfg.Repositories[0].Concurrency = 2
	// The second run must fail before emitting an implementation result.
	testkit.ReplacePhaseScript(t, cfg, workflow.Implement, `case "$(git branch --show-current)" in mergeyard/issue-8) exit 1;; esac
`+testkit.LoopScript(`printf fixed > feature.txt; `+testkit.FixedReport))
	g := &testkit.CreditLostPushGit{Manager: testkit.TestGitManager(rt.Workspace), LoseAck: true}
	h := &testkit.CreditHarness{ClassifiedHarness: &testkit.ClassifiedHarness{HarnessAdapter: harness.NewClaude(cfg.Agents.Claude), Outcome: harness.FailureClassification{Kind: harness.CreditsExhausted}}, Detection: true}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Git: g, Runner: r, Harnesses: map[string]harness.HarnessAdapter{"claude": h}})
	if err != nil {
		t.Fatal(err)
	}
	saved := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Fix)
	if saved.Fix.Status != "succeeded" || saved.Fix.CommitSHA == "" {
		t.Fatalf("saved fix fixture: %+v", saved)
	}
	api.Issues["owner/repo"] = append(api.Issues["owner/repo"], testkit.Ready(8))
	testkit.WaitFor(t, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range runs {
			if run.IssueNumber == 8 {
				return run.State == workflow.NeedsAttention
			}
		}
		return false
	})
	g.LoseAck = false
	selected, err := s.Retry(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.CreditProbes) != 0 {
		t.Fatalf("saved fix completion reserved an unused probe: %+v", selected.CreditProbes)
	}
	testkit.WaitFor(t, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		got, err := rt.Workflow.Get(context.Background(), saved.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.Phase == workflow.Review && got.State == workflow.WaitingForHarness
	})
	states, err := s.HarnessAvailability(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range states {
		if state.Harness == "claude" && (state.Available || state.ProbeID != "") {
			t.Fatalf("saved fix changed credit block: %+v", state)
		}
	}
}

func TestExpiredTemporaryResetDoesNotDescribeCreditProbeDelay(t *testing.T) {
	t.Parallel()

	_, rt, api, _, cfg, r := testkit.TmuxFlow(t, `exit 1`)
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	h := &testkit.CreditHarness{ClassifiedHarness: &testkit.ClassifiedHarness{HarnessAdapter: harness.NewClaude(cfg.Agents.Claude), Outcomes: []harness.FailureClassification{{Kind: harness.CreditsExhausted}, {Kind: harness.TemporaryLimit, ResetAt: reset}}}, Detection: true}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{"claude": h}})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
	if _, err := s.Retry(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	testkit.Finish(t, s, workflow.WaitingForHarness, workflow.Implement)
	now = reset
	states, err := s.HarnessAvailability(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range states {
		if state.Harness == "claude" && (state.Available || !state.ResetAt.IsZero()) {
			t.Fatalf("expired delay presented as future wait: %+v", state)
		}
	}
	limit, err := workflow.LoadHarnessLimit(context.Background(), rt.DB, "claude")
	if err != nil || !limit.ResetAt.Equal(reset) {
		t.Fatalf("expired reset authority lost: %+v %v", limit, err)
	}
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, testkit.ApiRequest(t, rt.Workspace.Root, http.MethodGet, "http://127.0.0.1:7331/api/status"))
	var snapshot struct {
		Runs []struct {
			HarnessWait map[string]json.RawMessage `json:"harness_wait"`
			History     []workflow.HarnessWait     `json:"harness_wait_history"`
		} `json:"runs"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &snapshot) != nil || len(snapshot.Runs) != 1 {
		t.Fatalf("run JSON: %d %s", response.Code, response.Body.String())
	}
	if _, present := snapshot.Runs[0].HarnessWait["reset_at"]; present {
		t.Fatalf("current credit wait JSON retained expired reset: %s", response.Body.String())
	}
	if len(snapshot.Runs[0].History) != 2 || !snapshot.Runs[0].History[1].ResetAt.Equal(reset) {
		t.Fatalf("observed reset history lost: %+v", snapshot.Runs[0].History)
	}
	response = httptest.NewRecorder()
	server.ServeHTTP(response, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/", nil))
	if response.Code != 200 || strings.Contains(response.Body.String(), "Probe may run after temporary reset") {
		t.Fatalf("expired delay displayed: %d %s", response.Code, response.Body.String())
	}
}

func TestCreditRetryEligibilityUsesSelectedHarness(t *testing.T) {
	t.Parallel()

	for _, blocked := range []string{"claude", "codex"} {
		t.Run(blocked, func(t *testing.T) {
			t.Parallel()

			_, rt, api, remote, cfg, r := testkit.TmuxPairingFlow(t, "claude", "codex", `printf fixed > feature.txt; `+testkit.FixedReport)
			cfg.MaxRounds = 1
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			primary := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Review)
			s = testkit.AddBusyAccountProbe(t, rt, api, remote, cfg, r, blocked)
			runs, err := s.Runs(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, run := range runs {
				if run.ID == primary.ID && run.RetryEligible != (blocked == "codex") {
					t.Fatalf("eligibility used reviewer rather than selected fixer: %+v", run)
				}
			}
			server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			page := httptest.NewRecorder()
			server.ServeHTTP(page, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/"+primary.ID, nil))
			if page.Code != 200 || strings.Contains(page.Body.String(), ">Retry run<") != (blocked == "codex") {
				t.Fatalf("selected-harness dashboard action: %d %s", page.Code, page.Body.String())
			}
			if blocked == "codex" {
				selected, err := s.Retry(context.Background(), primary.ID)
				if err != nil || selected.Phase != workflow.Fix || len(selected.CreditProbes) != 0 {
					t.Fatalf("other harness Retry blocked: %+v %v", selected, err)
				}
			}
		})
	}
}

func TestCreditProbeDoesNotHideCIOnlyDashboardRetry(t *testing.T) {
	t.Parallel()

	_, rt, api, remote, cfg, r := testkit.TmuxFlow(t, testkit.ReviewScript(testkit.ApprovedReview))
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	primary := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	if _, err := rt.Workflow.Transition(context.Background(), primary.ID, workflow.Request{Trigger: workflow.OperationFailed, Failure: &fault.Error{Code: "ci.wait_timeout", Message: "CI wait timed out"}}); err != nil {
		t.Fatal(err)
	}
	s = testkit.AddBusyAccountProbe(t, rt, api, remote, cfg, r, "claude")
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	page := httptest.NewRecorder()
	server.ServeHTTP(page, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/"+primary.ID, nil))
	if page.Code != 200 || !strings.Contains(page.Body.String(), ">Retry run<") {
		t.Fatalf("saved approval missing from eligibility snapshot: %d %s", page.Code, page.Body.String())
	}
	selected, err := s.Retry(context.Background(), primary.ID)
	if err != nil || selected.State != workflow.WaitingForCI || len(selected.CreditProbes) != 0 {
		t.Fatalf("CI-only retry reserved blocked harness: %+v %v", selected, err)
	}
}

func TestCreditRetryEligibilityIgnoresHistoricalCIHead(t *testing.T) {
	t.Parallel()

	for _, blocked := range []string{"claude", "codex"} {
		t.Run(blocked, func(t *testing.T) {
			t.Parallel()

			_, rt, base, remote, cfg, r := testkit.TmuxPairingFlow(t, "claude", "codex", testkit.DisputedReport)
			cfg.MaxRounds = 2
			script := strings.Replace(testkit.CiFixScript(`printf repaired > feature.txt; `+testkit.CiFixedReport), "*review-*)", "*review-2-*) "+testkit.ChangesReview+";;\n *review-*)", 1)
			testkit.ReplacePhaseScript(t, cfg, workflow.Fix, script)
			testkit.ReplacePhaseScript(t, cfg, workflow.Review, script)
			api := &testkit.CiGitHub{FakeGitHub: base, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "failure"}}}}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: testkit.CiBranchGitHub{CiGitHub: api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			primary := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Review)
			if primary.ReviewRound != 2 || primary.CI == nil || primary.CI.CurrentHead == primary.Review.TargetSHA {
				t.Fatalf("historical CI fixture: %+v", primary)
			}
			s = testkit.AddBusyAccountProbe(t, rt, base, remote, cfg, r, blocked)
			runs, err := s.Runs(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, run := range runs {
				if run.ID == primary.ID && run.RetryEligible != (blocked == "codex") {
					t.Fatalf("historical CI selected wrong harness: %+v", run)
				}
			}
			if blocked == "codex" {
				selected, err := s.Retry(context.Background(), primary.ID)
				if err != nil || selected.Phase != workflow.Fix || len(selected.CreditProbes) != 0 {
					t.Fatalf("historical CI blocked free fixer: %+v %v", selected, err)
				}
			}
		})
	}
}
