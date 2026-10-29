package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestReportedAccountResetWinsOverCooldownInEitherOrder(t *testing.T) {
	t.Parallel()

	for _, reportedFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "reported-first", false: "cooldown-first"}[reportedFirst], func(t *testing.T) {
			t.Parallel()

			_, runtime, api, _, cfg, r := testkit.TmuxFlow(t, `exit 1`)
			cfg.Concurrency = 2
			cfg.Repositories[0].Concurrency = 2
			api.Issues["owner/repo"] = append(api.Issues["owner/repo"], testkit.Ready(8))
			now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
			reportedReset := now.Add(5 * time.Minute)
			outcomes := []harness.FailureClassification{{Kind: harness.TemporaryLimit, ResetAt: reportedReset, Source: "reported-fixture"}, {Kind: harness.TemporaryLimit, Source: "no-reset-fixture"}}
			if !reportedFirst {
				outcomes[0], outcomes[1] = outcomes[1], outcomes[0]
			}
			fake := &testkit.ClassifiedHarness{HarnessAdapter: harness.NewClaude(cfg.Agents.Claude), Outcomes: outcomes}
			deps := scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{"claude": fake}}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), deps)
			if err != nil {
				t.Fatal(err)
			}
			testkit.WaitFor(t, func() bool {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				runs, err := s.Runs(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				return len(runs) == 2 && runs[0].State == workflow.WaitingForHarness && runs[1].State == workflow.WaitingForHarness
			})
			availability, err := s.HarnessAvailability(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !availability[0].ResetAt.Equal(reportedReset) || availability[0].ResetTimeSource != "reported" || availability[0].Source != "reported-fixture" {
				t.Fatalf("reported reset lost to cooldown: %+v", availability[0])
			}
			runs, err := s.Runs(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, run := range runs {
				if !run.HarnessWait.ResetAt.Equal(reportedReset) || run.HarnessWait.ResetTimeSource != "reported" {
					t.Fatalf("run retained non-authoritative cooldown: %+v", run.HarnessWait)
				}
			}
			history, err := runtime.Events.History(context.Background(), 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			limitedEvents := 0
			for _, event := range history {
				if event.Type == "harness.usage_limited" {
					limitedEvents++
					if event.RunID != "" {
						t.Fatal("harness event must be application-wide")
					}
				}
			}
			expected := 1
			if !reportedFirst {
				expected = 2
			}
			if limitedEvents != expected {
				t.Fatalf("account changes emitted %d events, want %d", limitedEvents, expected)
			}
			root := runtime.Workspace.Root
			if err := runtime.Close(); err != nil {
				t.Fatal(err)
			}
			runtime, err = app.Open(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			reopened := runtime
			t.Cleanup(func() { reopened.Close() })
			now = reportedReset
			s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), deps)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			runs, err = s.Runs(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, run := range runs {
				if run.State != workflow.Active || run.Implementer.Attempt != 1 {
					t.Fatalf("reported reset did not release both runs without a duplicate attempt: %+v", run)
				}
			}
			for range 3 {
				if err := runtime.Workflow.ReconcileHarnessLimits(context.Background(), now); err != nil {
					t.Fatal(err)
				}
			}
			history, err = runtime.Events.History(context.Background(), 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			availableEvents := 0
			for _, event := range history {
				if event.Type == "harness.available" {
					availableEvents++
				}
			}
			if availableEvents != 1 {
				t.Fatalf("expiry duplicated availability events: %d", availableEvents)
			}
		})
	}
}

func TestIncompleteNativeExecutionReachesAdapterClassifier(t *testing.T) {
	t.Parallel()

	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			t.Parallel()

			_, runtime, api, _, cfg, r := testkit.TmuxFlow(t, `exit 0`)
			adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
			if agent == "codex" {
				executable := filepath.Join(t.TempDir(), "codex")
				if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+testkit.CodexIdentity), 0700); err != nil {
					t.Fatal(err)
				}
				cfg.Agents.Codex.Executable = executable
				cfg.Repositories[0].Implementer.Agent = agent
				adapter = harness.NewCodex(cfg.Agents.Codex)
			}
			fake := &testkit.ClassifiedHarness{HarnessAdapter: adapter, Outcome: harness.FailureClassification{Kind: harness.TemporaryLimit, Source: "classified-fixture"}}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{agent: fake}})
			if err != nil {
				t.Fatal(err)
			}
			var run workflow.Run
			testkit.WaitFor(t, func() bool {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				runs, err := s.Runs(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if len(runs) != 1 || runs[0].Implementer == nil {
					return false
				}
				run = runs[0]
				return run.Implementer.Status != "running"
			})
			if fake.Calls != 1 || run.State != workflow.WaitingForHarness {
				t.Fatalf("incomplete native result skipped adapter classifier: calls=%d state=%s error=%s", fake.Calls, run.State, run.LastErrorCode)
			}
		})
	}
}

func TestSuccessfulNativeInvalidReportsBypassClassifier(t *testing.T) {
	t.Parallel()

	for _, agent := range []string{"claude", "codex"} {
		for _, status := range []string{"blocked", "failed", "invalid", "missing"} {
			t.Run(agent+"/"+status, func(t *testing.T) {
				t.Parallel()

				report := `{"schema_version":4,"verification":[],"criteria":[],"human_action":null,"status":"` + status + `","summary":"A tool mentioned a usage limit"}`
				if status == "blocked" {
					report = testkit.BlockedReport(report)
				}
				script := `printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":` + report + `}'`
				if status == "missing" {
					script = `printf '%s\n' '{"type":"result","subtype":"success","is_error":false}'`
				}
				_, runtime, api, _, cfg, r := testkit.TmuxFlow(t, script)
				adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
				if agent == "codex" {
					executable := filepath.Join(t.TempDir(), "codex")
					script = testkit.CodexIdentity + "\n" + testkit.CodexStructuredResult(report)
					if status == "missing" {
						script = testkit.CodexIdentity + "\nprintf '%s\\n' '{\"type\":\"turn.completed\"}'"
					}
					if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+script), 0700); err != nil {
						t.Fatal(err)
					}
					cfg.Agents.Codex.Executable = executable
					cfg.Repositories[0].Implementer.Agent = agent
					adapter = harness.NewCodex(cfg.Agents.Codex)
				}
				fake := &testkit.ClassifiedHarness{HarnessAdapter: adapter, Outcome: harness.FailureClassification{Kind: harness.TemporaryLimit}}
				s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{agent: fake}})
				if err != nil {
					t.Fatal(err)
				}
				done := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
				if fake.Calls != 0 || done.HarnessWait != nil {
					t.Fatalf("successful native response classified a limit: calls=%d run=%+v", fake.Calls, done)
				}
			})
		}
	}
}

func TestMixedRolesWaitBeforeFirstReviewExecution(t *testing.T) {
	t.Parallel()

	for _, limited := range []string{"claude", "codex"} {
		t.Run(limited, func(t *testing.T) {
			t.Parallel()

			other := "codex"
			if limited == "codex" {
				other = "claude"
			}
			_, runtime, api, remote, cfg, r := testkit.TmuxPairingFlow(t, limited, other, testkit.DisputedReport)
			// One run encounters the account limit; the other needs it only for review.
			script := `case "$*" in *implement-0-1*) printf partial > partial.txt; exit 1;; esac
` + testkit.ReviewScript(testkit.ApprovedReview)
			testkit.ReplacePhaseScript(t, cfg, workflow.Fix, script)
			otherScript := testkit.ReviewScript(testkit.ApprovedReview)
			executable := cfg.Agents.Claude.Executable
			if other == "codex" {
				executable = cfg.Agents.Codex.Executable
				otherScript = testkit.NativeCodexScript(otherScript)
			}
			if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+otherScript), 0700); err != nil {
				t.Fatal(err)
			}
			cfg.Concurrency = 2
			cfg.Repositories[0].Concurrency = 1
			mixed := cfg.Repositories[0]
			mixed.Repo = "owner/mixed"
			mixed.Implementer.Agent = other
			mixed.Reviewer.Agent = limited
			cfg.Repositories = append(cfg.Repositories, mixed)
			api.Issues[mixed.Repo] = []github.Issue{testkit.Ready(8)}
			testkit.GitCommand(t, filepath.Dir(remote), "config", "--file", testkit.TestGitConfig(t), "--add", "url."+remote+".insteadOf", "https://github.com/owner/mixed.git")
			now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
			adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
			if limited == "codex" {
				adapter = harness.NewCodex(cfg.Agents.Codex)
			}
			fake := &testkit.ClassifiedHarness{HarnessAdapter: adapter, Outcome: harness.FailureClassification{Kind: harness.TemporaryLimit, Source: "account-fixture"}}
			deps := scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{limited: fake}}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), deps)
			if err != nil {
				t.Fatal(err)
			}
			var waiting workflow.Run
			testkit.WaitForWithin(t, 10*time.Second, func() bool {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				runs, err := s.Runs(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				for _, run := range runs {
					if run.Repository == mixed.Repo {
						waiting = run
						return run.State == workflow.WaitingForHarness && run.Phase == workflow.Review
					}
				}
				return false
			})
			if waiting.HarnessWait.AttemptID != "" || waiting.Review != nil || waiting.Implementer.Agent != other {
				t.Fatalf("pre-launch gate incorrectly started review: %+v", waiting)
			}
			phases, err := filepath.Glob(filepath.Join(runtime.Workspace.Root, "runs", waiting.ID, "phases", "review-*"))
			if err != nil || len(phases) != 0 {
				t.Fatalf("review execution artifacts exist: %v %v", phases, err)
			}
			server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			path := "/runs/" + waiting.ID
			response := httptest.NewRecorder()
			server.ServeHTTP(response, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331"+path, nil))
			if response.Code != 200 || !strings.Contains(response.Body.String(), "Account gate; no execution started.") || strings.Contains(response.Body.String(), "consecutive 0 / allowance 0") {
				t.Errorf("misleading pre-launch wait on %s: %d %s", path, response.Code, response.Body.String())
			}
			root := runtime.Workspace.Root
			if err := runtime.Close(); err != nil {
				t.Fatal(err)
			}
			runtime, err = app.Open(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			reopened := runtime
			t.Cleanup(func() { reopened.Close() })
			s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), deps)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			saved, err := runtime.Workflow.Get(context.Background(), waiting.ID)
			if err != nil || saved.State != workflow.WaitingForHarness || saved.Review != nil {
				t.Fatalf("pre-launch wait lost on restart: %+v %v", saved, err)
			}
			now = waiting.HarnessWait.ResetAt
			testkit.WaitForWithin(t, 15*time.Second, func() bool {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				saved, err = runtime.Workflow.Get(context.Background(), waiting.ID)
				if err != nil {
					t.Fatal(err)
				}
				return saved.State == workflow.WaitingForCI
			})
			if saved.Review.Attempt != 1 || saved.ReviewRound != 1 || saved.Review.SessionID == saved.Implementer.SessionID || fake.Calls != 1 {
				t.Fatalf("mixed-role recovery duplicated or shared a conversation: %+v calls=%d", saved, fake.Calls)
			}
			input, err := os.ReadFile(filepath.Join(root, "runs", waiting.ID, "phases", "review-1-1", "input.md"))
			if err != nil || strings.Contains(string(input), "## Interruption context") {
				t.Fatalf("first reviewer inherited interruption context: %s %v", input, err)
			}
		})
	}
}
