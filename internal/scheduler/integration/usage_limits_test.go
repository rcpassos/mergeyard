package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestTemporaryLimitPreservesWorkAndResumesAfterRestart(t *testing.T) {
	t.Parallel()

	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			t.Parallel()

			script := `case "$*" in *implement-0-1*) printf partial > partial.txt; exit 1;; *) ` + testkit.SuccessfulScript + `;; esac`
			_, runtime, api, _, cfg, r := testkit.TmuxFlow(t, script)
			adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
			if agent == "codex" {
				executable := filepath.Join(t.TempDir(), "codex")
				script = testkit.CodexIdentity + "\n" + `case "$*" in *implement-0-1*) printf partial > partial.txt; exit 1;; *) printf implemented > feature.txt; ` + testkit.CodexResult + `;; esac`
				if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+script), 0700); err != nil {
					t.Fatal(err)
				}
				cfg.Agents.Codex.Executable = executable
				cfg.Repositories[0].Implementer.Agent = agent
				adapter = harness.NewCodex(cfg.Agents.Codex)
			}
			now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
			reset := now.Add(time.Hour)
			fake := &testkit.ClassifiedHarness{HarnessAdapter: adapter, Outcome: harness.FailureClassification{Kind: harness.TemporaryLimit, ResetAt: reset, Source: "fixture"}}
			deps := scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{agent: fake}}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), deps)
			if err != nil {
				t.Fatal(err)
			}
			waiting := testkit.Finish(t, s, workflow.WaitingForHarness, workflow.Implement)
			if waiting.HarnessWait == nil || waiting.HarnessWait.ResetTimeSource != "reported" || !waiting.HarnessWait.ResetAt.Equal(reset) || waiting.Implementer.Status != "usage_limited" {
				t.Fatalf("waiting: %+v", waiting)
			}
			server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"/runs/" + waiting.ID, "/settings", "/"} {
				response := httptest.NewRecorder()
				server.ServeHTTP(response, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331"+path, nil))
				for _, value := range []string{"Waiting until", "reported", "fixture"} {
					if response.Code != 200 || !strings.Contains(response.Body.String(), value) {
						t.Fatalf("%s missing %s: %d %s", path, value, response.Code, response.Body.String())
					}
				}
				// Every page's header carries one pill per harness: the limited
				// one with its reset time, the other one available.
				other := map[string]string{"claude": "codex", "codex": "claude"}[agent]
				for _, pill := range []string{agent + ` · Usage limited · resets <time datetime="` + reset.Format(time.RFC3339) + `"`, other + " · Available</span></summary>"} {
					if !strings.Contains(response.Body.String(), pill) {
						t.Fatalf("%s missing pill %s: %s", path, pill, response.Body.String())
					}
				}
			}
			status := httptest.NewRecorder()
			server.ServeHTTP(status, testkit.ApiRequest(t, runtime.Workspace.Root, http.MethodGet, "http://127.0.0.1:7331/api/status"))
			var operational web.Status
			if status.Code != 200 || json.Unmarshal(status.Body.Bytes(), &operational) != nil || len(operational.Harnesses) != 2 || operational.Runs[0].HarnessWait == nil {
				t.Fatalf("status %d %s", status.Code, status.Body.String())
			}
			if len(fake.Observed) != 1 || !fake.Observed[0].Equal(now) {
				t.Fatalf("classifier observation time: %v", fake.Observed)
			}
			session := waiting.Implementer.SessionID
			paths, _ := filepath.Glob(filepath.Join(runtime.Workspace.Root, "worktrees", "*", "*"))
			if len(paths) != 1 {
				t.Fatalf("worktrees: %v", paths)
			}
			if data, err := os.ReadFile(filepath.Join(paths[0], "partial.txt")); err != nil || string(data) != "partial" {
				t.Fatalf("partial work: %s %v", data, err)
			}
			workspaceRoot := runtime.Workspace.Root
			if err := runtime.Close(); err != nil {
				t.Fatal(err)
			}
			runtime, err = app.Open(context.Background(), workspaceRoot)
			if err != nil {
				t.Fatal(err)
			}
			reopened := runtime
			t.Cleanup(func() { reopened.Close() })
			s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), deps)
			if err != nil {
				t.Fatal(err)
			}
			for range 3 {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			runs, _ := s.Runs(context.Background())
			if runs[0].Implementer.Attempt != 1 || api.Creations != 0 {
				t.Fatal("restart launched before reset")
			}
			now = reset
			resumed := testkit.Finish(t, s, workflow.Active, workflow.Review)
			if resumed.Implementer.Attempt != 2 || resumed.Implementer.SessionID != session || !fake.Phases[1].Resume || fake.Calls != 1 || resumed.ReviewRound != 1 {
				t.Fatalf("resumption: %+v calls %d", resumed, fake.Calls)
			}
			history, err := runtime.Events.History(context.Background(), 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			limitedEvents, availableEvents := 0, 0
			for _, event := range history {
				if event.Type == "harness.usage_limited" {
					limitedEvents++
				}
				if event.Type == "harness.available" {
					availableEvents++
				}
			}
			if limitedEvents != 1 || availableEvents != 1 {
				t.Fatalf("duplicate harness events: %d %d", limitedEvents, availableEvents)
			}
			input, err := os.ReadFile(filepath.Join(runtime.Workspace.Root, "runs", waiting.ID, "phases", "implement-0-2", "input.md"))
			if err != nil || !strings.Contains(string(input), "usage limit") {
				t.Fatalf("interruption context: %s %v", input, err)
			}
		})
	}
}

func TestTemporaryLimitRestoresReviewerAndResumesSameRound(t *testing.T) {
	t.Parallel()

	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			t.Parallel()

			script := testkit.ReviewScript(`case "$*" in *review-1-1*) printf contaminated > feature.txt; printf untracked > reviewer.txt; exit 1;; *) ` + testkit.ApprovedReview + `;; esac`)
			_, runtime, api, _, cfg, r := testkit.TmuxFlow(t, script)
			adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
			if agent == "codex" {
				executable := filepath.Join(t.TempDir(), "codex")
				script = testkit.CodexIdentity + "\n" + `case "$*" in *review-1-1*) printf contaminated > feature.txt; printf untracked > reviewer.txt; exit 1;; *) ` + testkit.CodexStructuredResult(`{"schema_version":4,"criteria":[],"human_action":null,"prior_findings":[],"status":"approved","summary":"Safe","findings":[]}`) + `;; esac`
				if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+script), 0700); err != nil {
					t.Fatal(err)
				}
				cfg.Agents.Codex.Executable = executable
				cfg.Repositories[0].Reviewer.Agent = agent
				adapter = harness.NewCodex(cfg.Agents.Codex)
			}
			now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
			fake := &testkit.ClassifiedHarness{HarnessAdapter: adapter, Outcome: harness.FailureClassification{Kind: harness.TemporaryLimit, Source: "fixture"}}
			deps := scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{agent: fake}}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), deps)
			if err != nil {
				t.Fatal(err)
			}
			waiting := testkit.Finish(t, s, workflow.WaitingForHarness, workflow.Review)
			if waiting.Review == nil || !waiting.Review.Restored || !waiting.Review.Contaminated || waiting.HarnessWait.ResetTimeSource != "default_cooldown" || !waiting.HarnessWait.ResetAt.Equal(now.Add(30*time.Minute)) {
				t.Fatalf("restoration/wait: %+v", waiting)
			}
			paths, _ := filepath.Glob(filepath.Join(runtime.Workspace.Root, "worktrees", "*", "*"))
			data, err := os.ReadFile(filepath.Join(paths[0], "feature.txt"))
			if err != nil || string(data) != "implemented\n" {
				t.Fatalf("restored content %s %v", data, err)
			}
			if _, err := os.Stat(filepath.Join(paths[0], "reviewer.txt")); !os.IsNotExist(err) {
				t.Fatal("reviewer edit survived")
			}
			session := waiting.Review.SessionID
			now = now.Add(30 * time.Minute)
			s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), deps)
			if err != nil {
				t.Fatal(err)
			}
			done := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
			if done.ReviewRound != 1 || done.Review.Attempt != 2 || done.Review.SessionID != session || done.ApprovedSHA == "" || fake.Calls != 1 {
				t.Fatalf("review resume: %+v calls=%d", done, fake.Calls)
			}
		})
	}
}

func TestTemporaryLimitRetryGrantsExactlyOneWait(t *testing.T) {
	t.Parallel()

	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			t.Parallel()

			_, runtime, api, _, cfg, r := testkit.TmuxPairingFlow(t, agent, agent, testkit.DisputedReport)
			testkit.ReplacePhaseScript(t, cfg, workflow.Implement, `printf partial > partial.txt; exit 1`)
			cfg.UsageLimits.MaxWaits = 2
			now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
			adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
			if agent == "codex" {
				adapter = harness.NewCodex(cfg.Agents.Codex)
			}
			fake := &testkit.ClassifiedHarness{HarnessAdapter: adapter, Outcome: harness.FailureClassification{Kind: harness.TemporaryLimit, Source: "fixture"}}
			deps := scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{agent: fake}}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), deps)
			if err != nil {
				t.Fatal(err)
			}
			waiting := testkit.Finish(t, s, workflow.WaitingForHarness, workflow.Implement)
			now = waiting.HarnessWait.ResetAt
			attention := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
			if attention.LastErrorCode != "harness.usage_limit_waits_exhausted" || attention.HarnessWait.Consecutive != 2 {
				t.Fatalf("exhaustion: %+v", attention)
			}
			retried, err := s.Retry(context.Background(), attention.ID)
			if err != nil {
				t.Fatal(err)
			}
			if retried.State != workflow.WaitingForHarness || len(retried.Retries) != 1 || retried.Retries[0].GrantedWait != 3 || len(retried.HarnessWaitHistory) != 2 {
				t.Fatalf("grant: %+v", retried)
			}
			for range 3 {
				again, err := s.Retry(context.Background(), attention.ID)
				if err != nil || len(again.Retries) != 1 {
					t.Fatalf("duplicate grant: %+v %v", again, err)
				}
			}
			s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), deps)
			if err != nil {
				t.Fatal(err)
			}
			for range 3 {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			runs, _ := s.Runs(context.Background())
			if runs[0].Implementer.Attempt != 2 {
				t.Fatal("Retry bypassed reset")
			}
			now = retried.HarnessWait.ResetAt
			attention = testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
			if attention.Implementer.Attempt != 3 || attention.HarnessWait.Consecutive != 3 || attention.HarnessWait.Allowance != 3 || len(attention.Retries) != 1 {
				t.Fatalf("extra wait was unbounded: %+v", attention)
			}
		})
	}
}

func TestTemporaryLimitFixPreservesPhaseBudgetAndSettings(t *testing.T) {
	t.Parallel()

	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			t.Parallel()

			fix := `case "$*" in *fix-1-1*) printf partial > partial.txt; exit 1;; esac
printf fixed > feature.txt
` + testkit.FixedReport
			_, runtime, api, _, cfg, r := testkit.TmuxPairingFlow(t, agent, agent, fix)
			adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
			if agent == "codex" {
				adapter = harness.NewCodex(cfg.Agents.Codex)
			}
			now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
			fake := &testkit.ClassifiedHarness{HarnessAdapter: adapter, Outcome: harness.FailureClassification{Kind: harness.TemporaryLimit, Source: "fixture"}}
			deps := scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{agent: fake}}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), deps)
			if err != nil {
				t.Fatal(err)
			}
			waiting := testkit.Finish(t, s, workflow.WaitingForHarness, workflow.Fix)
			if waiting.ReviewRound != 1 || waiting.Fix.Attempt != 1 || waiting.Fix.Status != "usage_limited" {
				t.Fatalf("fix wait %+v", waiting)
			}
			now = waiting.HarnessWait.ResetAt
			done := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
			if done.ReviewRound != 2 || done.Fix.Attempt != 2 || done.Implementer.SessionID != waiting.Implementer.SessionID || fake.Calls != 1 {
				t.Fatalf("fix resume %+v", done)
			}
			var resumed *harness.PhaseContext
			for i := range fake.Phases {
				if fake.Phases[i].Phase == workflow.Fix && strings.Contains(fake.Phases[i].PhaseDir, "fix-1-2") {
					resumed = &fake.Phases[i]
				}
			}
			if resumed == nil || !resumed.Resume || resumed.SessionID != waiting.Implementer.SessionID {
				t.Fatalf("fix identity %+v", resumed)
			}
			input, err := os.ReadFile(filepath.Join(resumed.PhaseDir, "input.md"))
			if err != nil || !strings.Contains(string(input), "usage limit") || !strings.Contains(string(input), "F1") {
				t.Fatalf("fix context %s %v", input, err)
			}
		})
	}
}

func TestUsageLimitClassifierNeverSeesSuccessfulNativeResults(t *testing.T) {
	t.Parallel()

	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			t.Parallel()

			_, runtime, api, _, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
			adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
			if agent == "codex" {
				executable := filepath.Join(t.TempDir(), "codex")
				script := testkit.CodexIdentity + "\n" + `printf implemented > feature.txt` + "\n" + strings.ReplaceAll(testkit.CodexResult, "Added feature", "A tool mentioned a usage limit")
				if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+script), 0700); err != nil {
					t.Fatal(err)
				}
				cfg.Agents.Codex.Executable = executable
				cfg.Repositories[0].Implementer.Agent = agent
				adapter = harness.NewCodex(cfg.Agents.Codex)
			} else {
				text, err := os.ReadFile(cfg.Agents.Claude.Executable)
				if err != nil {
					t.Fatal(err)
				}
				// Reuse the captured successful warning event; no synthetic limit signal.
				fixture, err := os.ReadFile(testkit.RepositoryPath("docs/research/claude-m3/fixtures/allowed-warning/events.jsonl"))
				if err != nil {
					t.Fatal(err)
				}
				warning := ""
				for _, line := range strings.Split(string(fixture), "\n") {
					if strings.Contains(line, `"type":"rate_limit_event"`) {
						warning = line
					}
				}
				if warning == "" || strings.Contains(warning, "'") {
					t.Fatal("captured warning event unavailable")
				}
				text = []byte(strings.Replace(string(text), "#!/bin/sh\n", "#!/bin/sh\nprintf '%s\n' '"+warning+"'\n", 1))
				if err := os.WriteFile(cfg.Agents.Claude.Executable, text, 0700); err != nil {
					t.Fatal(err)
				}
			}
			fake := &testkit.ClassifiedHarness{HarnessAdapter: adapter, Outcome: harness.FailureClassification{Kind: harness.TemporaryLimit}}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{agent: fake}})
			if err != nil {
				t.Fatal(err)
			}
			done := testkit.Finish(t, s, workflow.Active, workflow.Review)
			if fake.Calls != 0 || done.HarnessWait != nil || done.Implementer.Attempt != 1 {
				t.Fatalf("false limit %+v calls=%d", done, fake.Calls)
			}
		})
	}
}

func TestOrdinaryFailureAfterLimitStillHasOrdinaryAttemptBudget(t *testing.T) {
	t.Parallel()

	_, runtime, api, _, cfg, r := testkit.TmuxFlow(t, `case "$*" in *implement-0-3*) `+testkit.SuccessfulScript+`;; *) exit 1;; esac`)
	cfg.Repositories[0].Implementer.MaxAttempts = 2
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	fake := &testkit.ClassifiedHarness{HarnessAdapter: harness.NewClaude(cfg.Agents.Claude), Outcome: harness.FailureClassification{Kind: harness.TemporaryLimit}}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{"claude": fake}})
	if err != nil {
		t.Fatal(err)
	}
	waiting := testkit.Finish(t, s, workflow.WaitingForHarness, workflow.Implement)
	fake.Outcome.Kind = harness.Ordinary
	now = waiting.HarnessWait.ResetAt
	done := testkit.Finish(t, s, workflow.Active, workflow.Review)
	input, err := os.ReadFile(filepath.Join(runtime.Workspace.Root, "runs", done.ID, "phases", "implement-0-3", "input.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(input), "## Interruption context") {
		t.Fatalf("ordinary retry has stale interruption note: %s", input)
	}
	resumed, err := os.ReadFile(filepath.Join(runtime.Workspace.Root, "runs", done.ID, "phases", "implement-0-2", "input.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resumed), "usage limit") || strings.Contains(string(resumed), "reviewer changes") {
		t.Fatalf("implementation interruption note describes unrelated reviewer work: %s", resumed)
	}
	if done.Implementer.Attempt != 3 || fake.Calls != 2 || len(done.HarnessWaitHistory) != 1 {
		t.Fatalf("ordinary attempt budget %+v calls=%d", done, fake.Calls)
	}
}

func TestTemporaryLimitGatesAcrossRepositoriesAndRetainsSlots(t *testing.T) {
	t.Parallel()

	for _, limited := range []string{"claude", "codex"} {
		for _, capacity := range []int{1, 3} {
			t.Run(fmt.Sprintf("%s/capacity-%d", limited, capacity), func(t *testing.T) {
				t.Parallel()

				_, runtime, api, remote, cfg, r := testkit.TmuxPairingFlow(t, limited, limited, testkit.DisputedReport)
				testkit.ReplacePhaseScript(t, cfg, workflow.Implement, `exit 1`)
				cfg.Repositories[0].Concurrency = 1
				now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
				adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
				if limited == "codex" {
					adapter = harness.NewCodex(cfg.Agents.Codex)
				}
				fake := &testkit.ClassifiedHarness{HarnessAdapter: adapter, Outcome: harness.FailureClassification{Kind: harness.TemporaryLimit}}
				ciAPI := &testkit.CiGitHub{FakeGitHub: api}
				deps := scheduler.Dependencies{GitHub: ciAPI, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{limited: fake}}
				s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), deps)
				if err != nil {
					t.Fatal(err)
				}
				waiting := testkit.Finish(t, s, workflow.WaitingForHarness, workflow.Implement)
				other := "codex"
				if limited == "codex" {
					other = "claude"
				}
				cfg.Concurrency = capacity
				sameRepo := cfg.Repositories[0]
				sameRepo.Repo = "owner/same"
				otherRepo := cfg.Repositories[0]
				otherRepo.Repo = "owner/other"
				otherRepo.Implementer.Agent = other
				otherRepo.Reviewer.Agent = other
				cfg.Repositories = append(cfg.Repositories, sameRepo, otherRepo)
				api.Issues["owner/repo"] = append(api.Issues["owner/repo"], testkit.Ready(8))
				api.Issues["owner/same"] = []github.Issue{testkit.Ready(9)}
				api.Issues["owner/other"] = []github.Issue{testkit.Ready(10), testkit.Ready(11)}
				configPath := testkit.TestGitConfig(t)
				for _, repo := range []string{"owner/same", "owner/other"} {
					testkit.GitCommand(t, filepath.Dir(remote), "config", "--file", configPath, "--add", "url."+remote+".insteadOf", "https://github.com/"+repo+".git")
				}
				// The eligible harness completes implementation and independent review.
				otherScript := testkit.ReviewScript(testkit.ApprovedReview)
				executable := cfg.Agents.Claude.Executable
				if other == "codex" {
					executable = cfg.Agents.Codex.Executable
					otherScript = testkit.NativeCodexScript(otherScript)
				}
				if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+otherScript), 0700); err != nil {
					t.Fatal(err)
				}
				s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), deps)
				if err != nil {
					t.Fatal(err)
				}
				testkit.WaitForWithin(t, 15*time.Second, func() bool {
					if err := s.Tick(context.Background()); err != nil {
						t.Fatal(err)
					}
					runs, err := s.Runs(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					return capacity == 1 || (len(runs) == 2 && runs[1].State == workflow.WaitingForCI)
				})
				runs, err := s.Runs(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				want := 1
				if capacity == 3 {
					want = 2
				}
				if len(runs) != want {
					t.Fatalf("capacity/gate allowed %d runs, want %d: %+v", len(runs), want, runs)
				}
				for _, run := range runs {
					if run.ID == waiting.ID {
						if run.State != workflow.WaitingForHarness || run.Implementer.Attempt != 1 {
							t.Fatalf("waiting slot lost %+v", run)
						}
						continue
					}
					if run.Repository != "owner/other" || run.IssueNumber != 10 || run.State != workflow.WaitingForCI {
						t.Fatalf("ineligible dispatch or other work stopped: %+v", run)
					}
				}
				// CI observes even while the first run keeps the limited harness gated.
				observed := 0
				ciAPI.AfterEvidence = func() { observed++ }
				for range 3 {
					if err := s.Tick(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				if capacity == 3 && observed == 0 {
					t.Fatal("CI observation stopped behind harness gate")
				}
			})
		}
	}
}

func TestUsageLimitResumeUsesExistingMissingSessionRecovery(t *testing.T) {
	t.Parallel()

	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			t.Parallel()

			_, runtime, api, _, cfg, r := testkit.TmuxPairingFlow(t, agent, agent, testkit.DisputedReport)
			script := `case "$*" in
 *implement-0-1*) printf partial > partial.txt; exit 1;;
 *implement-0-2*) printf 'No conversation found with session ID: %s\n' "$3" >&2; exit 1;;
 esac
` + testkit.ReviewScript(testkit.ApprovedReview)
			executable := cfg.Agents.Claude.Executable
			if agent == "codex" {
				executable = cfg.Agents.Codex.Executable
				script = testkit.NativeCodexScript(testkit.ReviewScript(testkit.ApprovedReview))
				injection := `case "$phase_dir" in
 *implement-0-1*) printf '%s\n' '{"type":"thread.started","thread_id":"` + testkit.CodexID + `"}'; printf partial > partial.txt; exit 1;;
 *implement-0-2*) echo 'Error: thread/resume: thread/resume failed: no rollout found for thread id ` + testkit.CodexID + ` (code -32600)' >&2; exit 1;;
 *implement-0-3*) identity='` + testkit.ReplacementID + `';;
 esac
`
				marker := ` printf '%s\n' "{\"type\":\"thread.started\",\"thread_id\":\"$identity\"}"`
				script = strings.Replace(script, marker, injection+marker, 1)
			}
			if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+script), 0700); err != nil {
				t.Fatal(err)
			}
			adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
			if agent == "codex" {
				adapter = harness.NewCodex(cfg.Agents.Codex)
			}
			fake := &testkit.ClassifiedHarness{HarnessAdapter: adapter, Outcome: harness.FailureClassification{Kind: harness.Ordinary}, Outcomes: []harness.FailureClassification{{Kind: harness.TemporaryLimit}}}
			now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{agent: fake}})
			if err != nil {
				t.Fatal(err)
			}
			waiting := testkit.Finish(t, s, workflow.WaitingForHarness, workflow.Implement)
			now = waiting.HarnessWait.ResetAt
			done := testkit.Finish(t, s, workflow.Active, workflow.Review)
			input, err := os.ReadFile(filepath.Join(runtime.Workspace.Root, "runs", done.ID, "phases", "implement-0-3", "input.md"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(input), "## Interruption context") {
				t.Fatalf("fresh recovery session has stale interruption note: %s", input)
			}
			if done.Implementer.Attempt != 3 || len(done.SessionRecoveries) != 1 || done.SessionRecoveries[0].PreviousSessionID != waiting.Implementer.SessionID || done.Implementer.SessionID == waiting.Implementer.SessionID {
				t.Fatalf("recovery %+v", done)
			}
		})
	}
}
