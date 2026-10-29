package integration_test

import (
	"context"
	"encoding/json"

	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestCreditRetrySelectedRunRecoversWithoutReset(t *testing.T) {
	t.Parallel()

	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			t.Parallel()

			_, runtime, api, _, cfg, r := testkit.TmuxPairingFlow(t, agent, agent, testkit.DisputedReport)
			testkit.ReplacePhaseScript(t, cfg, workflow.Implement, `case "$*" in *implement-0-1*) printf partial > partial.txt; exit 1;; esac
`+testkit.SuccessfulScript)
			adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
			if agent == "codex" {
				adapter = harness.NewCodex(cfg.Agents.Codex)
			}
			h := &testkit.CreditHarness{ClassifiedHarness: &testkit.ClassifiedHarness{HarnessAdapter: adapter, Outcome: harness.FailureClassification{Kind: harness.CreditsExhausted, Source: "classified-fixture"}}, Detection: true}
			now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
			deps := scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{agent: h}}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), deps)
			if err != nil {
				t.Fatal(err)
			}
			attention := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
			if attention.LastErrorCode != "harness.credits_exhausted" || attention.HarnessWait == nil || !attention.HarnessWait.ResetAt.IsZero() {
				t.Fatalf("credit attention: %+v", attention)
			}
			availability, err := s.HarnessAvailability(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range availability {
				if v.Harness == agent && (v.Available || !v.ResetAt.IsZero()) {
					t.Fatalf("credit block: %+v", v)
				}
			}
			server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			for _, enabled := range []bool{false, true} {
				h.Detection = enabled
				response := httptest.NewRecorder()
				server.ServeHTTP(response, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/"+attention.ID, nil))
				body := response.Body.String()
				if response.Code != 200 || !strings.Contains(body, "no scheduled reset") || strings.Contains(body, "0001-01-01") || strings.Contains(body, ">Retry run<") != enabled {
					t.Fatalf("credit controls capability=%v: %d %s", enabled, response.Code, body)
				}
			}
			h.Detection = false
			testkit.AssertCreditConflict(t, runtime.Workspace.Root, server, attention.ID, "harness.credit_detection_unavailable", "Credit recovery is unavailable")
			h.Detection = true
			now = now.Add(365 * 24 * time.Hour)
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			selected, err := s.Retry(context.Background(), attention.ID)
			if err != nil || selected.State != workflow.Active {
				t.Fatalf("Retry: %+v %v", selected, err)
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, testkit.ApiRequest(t, runtime.Workspace.Root, http.MethodGet, "http://127.0.0.1:7331/api/status"))
			var snapshot web.Status
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &snapshot) != nil || len(snapshot.Runs[0].CreditProbes) != 1 || snapshot.Runs[0].CreditProbes[0].Status != "reserved" || strings.Contains(response.Body.String(), "0001-01-01") {
				t.Fatalf("reserved probe status: %d %s", response.Code, response.Body.String())
			}
			s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), deps)
			if err != nil {
				t.Fatal(err)
			}
			done := testkit.Finish(t, s, workflow.Active, workflow.Review)
			if done.Implementer.Attempt != 2 || done.Implementer.SessionID != attention.Implementer.SessionID {
				t.Fatalf("continuity: %+v", done)
			}
			availability, err = s.HarnessAvailability(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range availability {
				if v.Harness == agent && !v.Available {
					t.Fatalf("recovery: %+v", v)
				}
			}
		})
	}
}

func TestCreditProbeRequiresModelCompletionAndHonorsCapability(t *testing.T) {
	t.Parallel()

	for _, response := range []string{"blocked", "failed", "incomplete", "credits", "unsupported"} {
		t.Run(response, func(t *testing.T) {
			t.Parallel()

			_, rt, api, _, cfg, r := testkit.TmuxFlow(t, `exit 1`)
			h := &testkit.CreditHarness{ClassifiedHarness: &testkit.ClassifiedHarness{HarnessAdapter: harness.NewClaude(cfg.Agents.Claude), Outcome: harness.FailureClassification{Kind: harness.CreditsExhausted}}, Detection: response != "unsupported"}
			deps := scheduler.Dependencies{GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{"claude": h}}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
			if err != nil {
				t.Fatal(err)
			}
			run := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
			if response == "unsupported" {
				if _, err := s.Retry(context.Background(), run.ID); err == nil {
					t.Fatal("offered unsupported credit recovery")
				}
				return
			}
			script := `printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"schema_version":4,"verification":[],"criteria":[],"human_action":null,"status":"` + response + `","summary":"Task needs attention"}}'`
			if response == "blocked" {
				script = testkit.BlockedReport(script)
			}
			if response == "incomplete" {
				script = `printf '%s\n' '{"type":"system","subtype":"init","session_id":"login-only"}'; exit 1`
				h.Outcome.Kind = harness.Ordinary
			}
			if response == "credits" {
				script = `exit 1`
			}
			testkit.ReplacePhaseScript(t, cfg, workflow.Implement, script)
			if _, err := s.Retry(context.Background(), run.ID); err != nil {
				t.Fatal(err)
			}
			testkit.WaitFor(t, func() bool {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				got, err := rt.Workflow.Get(context.Background(), run.ID)
				if err != nil {
					t.Fatal(err)
				}
				return got.State == workflow.NeedsAttention || got.State == workflow.WaitingForHarness
			})
			states, err := s.HarnessAvailability(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range states {
				if v.Harness == "claude" && (v.Available != (response == "blocked" || response == "failed") || v.ProbeID != "") {
					t.Fatalf("proof %s: %+v", response, v)
				}
			}
			got, err := rt.Workflow.Get(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.CreditProbes) != 1 {
				t.Fatalf("probe history: %+v", got)
			}
		})
	}
}

func TestCreditRecoveryAcrossReviewAndFixPairings(t *testing.T) {
	t.Parallel()

	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			for _, phase := range []workflow.Phase{workflow.Review, workflow.Fix} {
				t.Run(implementer+"/"+reviewer+"/"+string(phase), func(t *testing.T) {
					t.Parallel()

					_, rt, api, remote, cfg, r := testkit.TmuxPairingFlow(t, implementer, reviewer, `printf fixed > feature.txt; `+testkit.FixedReport)
					script := testkit.LoopScript(`case "$*" in *fix-1-1*) printf partial > partial.txt; exit 1;; esac
printf fixed > feature.txt; ` + testkit.FixedReport)
					agent := implementer
					if phase == workflow.Review {
						agent = reviewer
						script = testkit.ReviewScript(`case "$*" in *review-1-1*) printf tamper > feature.txt; printf reviewer > reviewer.txt; exit 1;; esac
` + testkit.ApprovedReview)
					}
					testkit.ReplacePhaseScript(t, cfg, phase, script)
					adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
					if agent == "codex" {
						adapter = harness.NewCodex(cfg.Agents.Codex)
					}
					h := &testkit.CreditHarness{ClassifiedHarness: &testkit.ClassifiedHarness{HarnessAdapter: adapter, Outcome: harness.FailureClassification{Kind: harness.CreditsExhausted}}, Detection: true}
					deps := scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r, Harnesses: map[string]harness.HarnessAdapter{agent: h}}
					s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
					if err != nil {
						t.Fatal(err)
					}
					attention := testkit.Finish(t, s, workflow.NeedsAttention, phase)
					if attention.LastErrorCode != "harness.credits_exhausted" || (phase == workflow.Review && (!attention.Review.Restored || !attention.Review.Contaminated || attention.Review.Accepted)) {
						t.Fatalf("interrupted phase: %+v", attention)
					}
					if _, err := s.Retry(context.Background(), attention.ID); err != nil {
						t.Fatal(err)
					}
					done := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
					if phase == workflow.Review && (done.ReviewRound != 1 || done.Review.Attempt != 2 || done.Review.SessionID != attention.Review.SessionID) {
						t.Fatalf("review continuity: %+v", done)
					}
					if phase == workflow.Fix && (done.Fix.Attempt != 2 || done.ReviewRound != 2 || done.Implementer.SessionID != attention.Implementer.SessionID) {
						t.Fatalf("fix continuity: %+v", done)
					}
					if len(done.CreditProbes) != 1 || done.CreditProbes[0].Status != "recovered" || api.Creations != 1 || testkit.GitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7") == "0" {
						t.Fatalf("selected recovery: %+v", done)
					}
				})
			}
		}
	}
}

func TestOlderProbeCompletionCannotClearNewCreditRestriction(t *testing.T) {
	t.Parallel()

	for _, kind := range []harness.FailureKind{harness.CreditsExhausted, harness.TemporaryLimit} {
		t.Run(string(kind), func(t *testing.T) { testkit.OlderProbeCompletion(t, kind) })
	}
}

func TestCreditRetryReconcilesWorkOnOtherHarness(t *testing.T) {
	t.Parallel()

	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			t.Parallel()

			other := "codex"
			if agent == "codex" {
				other = "claude"
			}
			_, rt, api, _, cfg, r := testkit.TmuxPairingFlow(t, agent, other, testkit.DisputedReport)
			testkit.ReplacePhaseScript(t, cfg, workflow.Fix, `printf partial > partial.txt; exit 1`)
			adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
			if agent == "codex" {
				adapter = harness.NewCodex(cfg.Agents.Codex)
			}
			h := &testkit.CreditHarness{ClassifiedHarness: &testkit.ClassifiedHarness{HarnessAdapter: adapter, Outcome: harness.FailureClassification{Kind: harness.CreditsExhausted}}, Detection: true}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{agent: h}})
			if err != nil {
				t.Fatal(err)
			}
			run := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
			// External work has published a PR while the automated implementer waits.
			paths, err := filepath.Glob(filepath.Join(rt.Workspace.Root, "worktrees", "*", "*"))
			if err != nil || len(paths) != 1 {
				t.Fatalf("worktrees %v %v", paths, err)
			}
			path := paths[0]
			testkit.GitCommand(t, path, "add", ".")
			testkit.GitCommand(t, path, "commit", "-m", "User completed partial work")
			testkit.GitCommand(t, path, "push", "origin", "HEAD")
			pr := &github.PullRequest{Number: 101, State: github.Open, Draft: true}
			pr.Head.Ref = "mergeyard/issue-7"
			pr.Head.SHA = testkit.GitCommand(t, path, "rev-parse", "HEAD")
			pr.Head.Repo.FullName = "owner/repo"
			pr.Base.Ref = "main"
			api.Prs = map[string]*github.PullRequest{pr.Head.Ref: pr}
			selected, err := s.Retry(context.Background(), run.ID)
			if err != nil || selected.State != workflow.Active || selected.Phase != workflow.Review || len(selected.CreditProbes) != 0 {
				t.Fatalf("reconciled Retry: %+v %v", selected, err)
			}
			testkit.WaitFor(t, func() bool {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				got, err := rt.Workflow.Get(context.Background(), run.ID)
				if err != nil {
					t.Fatal(err)
				}
				return got.State == workflow.WaitingForHarness && got.Phase == workflow.Fix && got.Review != nil && got.Review.Agent == other && got.Review.Status == "succeeded"
			})
			states, err := s.HarnessAvailability(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range states {
				if v.Harness == agent && (v.Available || v.ProbeID != "") {
					t.Fatalf("other harness cleared credit block: %+v", v)
				}
			}
			if err := s.Stop(context.Background(), run.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCreditRetryAfterTemporaryProbeWaitExhaustion(t *testing.T) {
	t.Parallel()

	_, rt, api, _, cfg, r := testkit.TmuxFlow(t, `exit 1`)
	cfg.UsageLimits.MaxWaits = 2
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	reported := now.Add(time.Hour)
	h := &testkit.CreditHarness{ClassifiedHarness: &testkit.ClassifiedHarness{HarnessAdapter: harness.NewClaude(cfg.Agents.Claude), Outcomes: []harness.FailureClassification{{Kind: harness.CreditsExhausted}, {Kind: harness.TemporaryLimit, ResetAt: reported}}, Outcome: harness.FailureClassification{Kind: harness.TemporaryLimit}}, Detection: true}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{"claude": h}})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
	if _, err := s.Retry(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	run = testkit.Finish(t, s, workflow.WaitingForHarness, workflow.Implement)
	if run.HarnessWait.Consecutive != 1 || !run.HarnessWait.ResetAt.Equal(reported) {
		t.Fatalf("temporary allowance/reset: %+v", run.HarnessWait)
	}
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), "Probe may run after temporary reset") {
		t.Fatalf("credit/temporary diagnostics: %d %s", response.Code, response.Body.String())
	}

	if _, err := s.Retry(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	run, err = rt.Workflow.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != workflow.WaitingForHarness || run.Implementer.Attempt != 2 || len(run.CreditProbes) != 2 || run.CreditProbes[1].Status != "reserved" {
		t.Fatalf("probe bypassed temporary reset: %+v", run)
	}
	again, err := s.Retry(context.Background(), run.ID)
	if err != nil || len(again.CreditProbes) != 2 {
		t.Fatalf("duplicate waiting reservation: %+v %v", again, err)
	}
	now = reported
	run = testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
	if run.LastErrorCode != "harness.usage_limit_waits_exhausted" || run.HarnessWait.Consecutive != 2 {
		t.Fatalf("wait exhaustion: %+v", run)
	}
	selected, err := s.Retry(context.Background(), run.ID)
	if err != nil || selected.State != workflow.Active || len(selected.CreditProbes) != 3 || selected.CreditProbes[2].Status != "reserved" {
		t.Fatalf("Retry did not reserve credit probe after wait exhaustion: %+v %v", selected, err)
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	run, err = rt.Workflow.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != workflow.WaitingForHarness || run.Implementer.Attempt != 3 {
		t.Fatalf("exhausted wait Retry bypassed cooldown: %+v", run)
	}
	testkit.ReplacePhaseScript(t, cfg, workflow.Implement, testkit.SuccessfulScript)
	now = run.HarnessWait.ResetAt
	done := testkit.Finish(t, s, workflow.Active, workflow.Review)
	if done.Implementer.Attempt != 4 || done.CreditProbes[2].Status != "recovered" {
		t.Fatalf("delayed recovery: %+v", done)
	}
}
