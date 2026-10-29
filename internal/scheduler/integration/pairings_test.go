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

	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestHarnessPairingsCompleteFixLoop(t *testing.T) {
	t.Parallel()

	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			t.Run(implementer+"/"+reviewer, func(t *testing.T) {
				t.Parallel()

				s, runtime, api, remote, _, _ := testkit.TmuxPairingFlow(t, implementer, reviewer, `printf fixed > feature.txt; `+testkit.FixedReport)
				run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
				if run.ReviewRound != 2 || run.Review.Agent != reviewer || run.Fix.Agent != implementer || run.Review.SessionID == run.Fix.SessionID || run.Fix.SessionID != run.Implementer.SessionID {
					t.Fatalf("role ownership: %+v review=%+v fix=%+v", run, run.Review, run.Fix)
				}
				if run.Review.Model != "review-model" || run.Fix.Model != "implement-model" || !run.Review.Accepted || len(run.Fix.Report.Responses) != 2 || len(run.Review.Report.PriorFindings) != 2 || run.Review.Report.PriorCounts().Fixed != 1 || run.Review.Report.PriorCounts().NoLongerApplies != 1 {
					t.Fatal("lost role settings or reports")
				}
				if api.Creations != 1 || testkit.GitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7") != "2" {
					t.Fatal("duplicated publication")
				}
				history, err := runtime.Events.History(context.Background(), 0, 100)
				if err != nil {
					t.Fatal(err)
				}
				resumed := 0
				for _, event := range history {
					if event.Type != "phase.attempt_started" {
						continue
					}
					var payload map[string]any
					if err := json.Unmarshal(event.Payload, &payload); err != nil {
						t.Fatal(err)
					}
					if payload["resumed_session"] == true {
						resumed++
					}
				}
				if resumed != 2 {
					t.Fatalf("expected resumed fix and review, got %d", resumed)
				}
			})
		}
	}
}

func TestHarnessPairingsStopAndRestart(t *testing.T) {
	t.Parallel()

	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			for _, phase := range []workflow.Phase{workflow.Review, workflow.Fix} {
				t.Run(implementer+"/"+reviewer+"/"+string(phase), func(t *testing.T) {
					t.Parallel()

					gate := filepath.Join(t.TempDir(), "release")
					held := `while [ ! -f '` + gate + `' ]; do /bin/sleep 0.02; done; `
					s, runtime, api, _, cfg, r := testkit.TmuxPairingFlow(t, implementer, reviewer, `printf fixed > feature.txt; `+testkit.FixedReport)
					if phase == workflow.Review {
						testkit.ReplacePhaseScript(t, cfg, phase, `case "$*" in *review-1-*) `+held+testkit.ChangesReview+`;; *review-*) `+testkit.ApprovedAfterFixReview+`;; *fix-*) printf fixed > feature.txt; `+testkit.FixedReport+`;; *) `+testkit.SuccessfulScript+`;; esac`)
					} else {
						testkit.ReplacePhaseScript(t, cfg, phase, testkit.LoopScript(held+`printf fixed > feature.txt; `+testkit.FixedReport))
					}
					run := testkit.WaitForPhase(t, s, phase)
					ref, err := s.Watch(context.Background(), run.ID)
					if err != nil {
						t.Fatal(err)
					}
					if phase == workflow.Review && run.Review.SessionID == run.Implementer.SessionID {
						t.Fatal("review reused implementer")
					}
					server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
					if err != nil {
						t.Fatal(err)
					}
					for _, path := range []string{"/api/status", "/runs/" + run.ID} {
						response := httptest.NewRecorder()
						request := testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331"+path, nil)
						testkit.SignAPIRequest(t, runtime.Workspace.Root, request)
						server.ServeHTTP(response, request)
						identity, model := run.Review.SessionID, "review-model"
						if phase == workflow.Fix {
							identity, model = run.Fix.SessionID, "implement-model"
						}
						if response.Code != 200 || !strings.Contains(response.Body.String(), identity) || !strings.Contains(response.Body.String(), model) {
							t.Fatalf("missing settings: %s", response.Body.String())
						}
					}
					restarted, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := restarted.Reconcile(context.Background()); err != nil {
						t.Fatal(err)
					}
					next, err := restarted.Watch(context.Background(), run.ID)
					if err != nil || next != ref {
						t.Fatalf("restart duplicated process: %+v %v", next, err)
					}
					if err := restarted.Stop(context.Background(), run.ID); err != nil {
						t.Fatal(err)
					}
					saved, err := runtime.Workflow.Get(context.Background(), run.ID)
					if err != nil || saved.State != workflow.Stopped || saved.Review.SessionID != run.Review.SessionID {
						t.Fatalf("lost stop: %+v %v", saved, err)
					}
					status, err := r.SessionStatus(context.Background(), ref)
					if err != nil || status.State == runner.SessionRunning {
						t.Fatalf("owned phase survived stop: %+v %v", status, err)
					}
					if api.Creations != 1 {
						t.Fatal("duplicated PR")
					}
				})
			}
		}
	}
}

func TestCodexReviewerIdentityBeforeNextPoll(t *testing.T) {
	t.Parallel()

	s, runtime, api, _, cfg, r := testkit.TmuxPairingFlow(t, "codex", "codex", testkit.DisputedReport)
	testkit.ReplacePhaseScript(t, cfg, workflow.Review, `case "$*" in *review-*) sleep 30; `+testkit.ApprovedReview+`;; *) `+testkit.SuccessfulScript+`;; esac`)
	run := testkit.Finish(t, s, workflow.Active, workflow.Review)
	cfg.PollInterval = time.Hour
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	defer func() { cancel(); <-done }()
	testkit.WaitFor(t, func() bool {
		saved, err := runtime.Workflow.Get(context.Background(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		return saved.Review != nil && saved.Review.SessionID == testkit.ReviewerCodexID && saved.Review.Status == "running"
	})
	if err := s.Stop(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCodexReviewRestoresMutationsAndPermitsBuildOutput(t *testing.T) {
	t.Parallel()

	for _, tamper := range []bool{false, true} {
		t.Run(fmt.Sprintf("tamper-%t", tamper), func(t *testing.T) {
			t.Parallel()

			s, runtime, _, _, cfg, _ := testkit.TmuxPairingFlow(t, "codex", "codex", testkit.DisputedReport)
			mutation := `/bin/mkdir -p build-output; printf cache > build-output/cache; `
			if tamper {
				mutation += `printf tamper > feature.txt; printf new > reviewer.txt; git add feature.txt; git -c user.name=Reviewer -c user.email=reviewer@example.invalid commit -m 'reviewer commit' >&2; `
			}
			testkit.ReplacePhaseScript(t, cfg, workflow.Review, testkit.ReviewScript(mutation+testkit.ApprovedReview))
			run := testkit.Finish(t, s, workflow.Active, workflow.Review)
			var path string
			if err := runtime.DB.QueryRow("SELECT worktree_path FROM runs WHERE id=?", run.ID).Scan(&path); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, ".gitignore"), []byte("build-output/\n"), 0600); err != nil {
				t.Fatal(err)
			}
			testkit.GitCommand(t, path, "add", ".gitignore")
			state := workflow.WaitingForCI
			if tamper {
				state = workflow.NeedsAttention
			}
			run = testkit.Finish(t, s, state, workflow.Review)
			if run.Review.Contaminated != tamper || !run.Review.Restored || run.Review.Accepted == tamper {
				t.Fatalf("integrity: %+v", run.Review)
			}
			if tamper && (run.ApprovedSHA != "" || run.LastErrorCode != "review.code_changed") {
				t.Fatal("contaminated verdict accepted")
			}
			if data, err := os.ReadFile(filepath.Join(path, "feature.txt")); err != nil || string(data) != "implemented\n" {
				t.Fatalf("lost original implementation: %s %v", data, err)
			}
			if data, err := os.ReadFile(filepath.Join(path, "build-output", "cache")); err != nil || string(data) != "cache" {
				t.Fatal("lost permitted build output")
			}
			if _, err := os.Stat(filepath.Join(path, "reviewer.txt")); !os.IsNotExist(err) {
				t.Fatal("reviewer file survived restoration")
			}
		})
	}
}

func TestCodexReviewAndFixRequireCompletionAndSchema(t *testing.T) {
	t.Parallel()

	for _, phase := range []workflow.Phase{workflow.Review, workflow.Fix} {
		for _, kind := range []string{"schema", "completion", "provider failure"} {
			t.Run(string(phase)+"/"+kind, func(t *testing.T) {
				t.Parallel()

				s, _, api, remote, cfg, _ := testkit.TmuxPairingFlow(t, "codex", "codex", `printf fixed > feature.txt; `+testkit.FixedReport)
				report := testkit.FixedReport
				if phase == workflow.Review {
					report = testkit.ChangesReview
				}
				if kind == "schema" {
					report = strings.ReplaceAll(report, `"schema_version":4,"criteria":[],"human_action":null,`, "")
				}
				if kind == "provider failure" {
					report = `echo 'Unsupported model' >&2; exit 1`
				}
				script := testkit.LoopScript(report)
				if phase == workflow.Review {
					script = testkit.ReviewScript(report)
				}
				script = testkit.NativeCodexScript(script)
				if kind == "completion" {
					script = strings.ReplaceAll(script, `printf '%s\n' '{"type":"turn.completed"}'`, `case "$phase_dir" in *`+string(phase)+`-*) ;; *) printf '%s\n' '{"type":"turn.completed"}';; esac`)
				}
				if err := os.WriteFile(cfg.Agents.Codex.Executable, []byte("#!/bin/sh\n"+script), 0700); err != nil {
					t.Fatal(err)
				}
				run := testkit.Finish(t, s, workflow.NeedsAttention, phase)
				code := "phase.result_invalid"
				if kind == "completion" {
					code = "phase.result_missing"
				}
				if kind == "provider failure" {
					code = "phase.execution_failed"
					if phase == workflow.Fix {
						code = "phase.retries_exhausted"
					}
				}
				if run.LastErrorCode != code || run.ApprovedSHA != "" || run.ReviewRound != 1 || api.Creations != 1 || testkit.GitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7") != "1" {
					t.Fatalf("native contract: %+v", run)
				}
			})
		}
	}
}

func TestCodexVerifiedMissingReviewOrFixStartsFreshBoundedConversation(t *testing.T) {
	t.Parallel()

	const freshID = "01a10c61-253c-7173-a0d7-d82901ccda96"
	for _, phase := range []workflow.Phase{workflow.Review, workflow.Fix} {
		t.Run(string(phase), func(t *testing.T) {
			t.Parallel()

			_, runtime, api, _, cfg, r := testkit.TmuxPairingFlow(t, "codex", "codex", `printf fixed > feature.txt; `+testkit.FixedReport)
			cfg.Repositories[0].Implementer.MaxAttempts = 2
			cfg.Repositories[0].Reviewer.MaxAttempts = 2
			script := testkit.NativeCodexScript(testkit.LoopScript(`printf fixed > feature.txt; ` + testkit.FixedReport))
			if phase == workflow.Review {
				// First attempt fails after discovery; retry resumes the captured identity,
				// then a verified missing-rollout signal permits fresh recovery.
				cfg.Repositories[0].Reviewer.MaxAttempts = 3
				injected := `case "$phase_dir" in
     *review-1-1*) printf '%s\n' "{\"type\":\"thread.started\",\"thread_id\":\"$identity\"}"; exit 1;;
     *review-1-2*) echo 'Error: thread/resume: thread/resume failed: no rollout found for thread id ` + testkit.ReviewerCodexID + ` (code -32600)' >&2; exit 1;;
     *review-1-3*|*review-2-*) identity='` + freshID + `';;
    esac
`
				script = strings.Replace(script, ` case "$phase_dir" in *fix-*|*review-2-*)`, injected+` case "$phase_dir" in *fix-*|*review-2-*)`, 1)
			} else {
				injected := `case "$phase_dir" in
     *fix-1-1*) echo 'Error: thread/resume: thread/resume failed: no rollout found for thread id ` + testkit.CodexID + ` (code -32600)' >&2; exit 1;;
     *fix-1-2*) identity='` + freshID + `';;
    esac
`
				script = strings.Replace(script, ` case "$phase_dir" in *fix-*|*review-2-*)`, injected+` case "$phase_dir" in *fix-1-1*|*review-2-*)`, 1)
			}
			if err := os.WriteFile(cfg.Agents.Codex.Executable, []byte("#!/bin/sh\n"+script), 0700); err != nil {
				t.Fatal(err)
			}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
			if phase == workflow.Review && (run.Review.SessionID != freshID || run.Review.SessionID == run.Fix.SessionID) {
				t.Fatalf("review recovery: %+v", run.Review)
			}
			if phase == workflow.Fix && (run.Fix.SessionID != freshID || run.Implementer.SessionID != freshID || len(run.FixHistory) != 2) {
				t.Fatalf("fix recovery: %+v", run.Fix)
			}
			for range 2 {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestCodexFreshFixLaunchFailureCanUseRemainingAttempt(t *testing.T) {
	t.Parallel()

	const freshID = "01a10c61-253c-7173-a0d7-d82901ccda96"
	_, runtime, api, remote, cfg, r := testkit.TmuxPairingFlow(t, "codex", "codex", `printf fixed > feature.txt; `+testkit.FixedReport)
	cfg.Repositories[0].Implementer.MaxAttempts = 3
	script := testkit.NativeCodexScript(testkit.LoopScript(`printf fixed > feature.txt; ` + testkit.FixedReport))
	injected := `case "$phase_dir" in
 *fix-1-1*) echo 'Error: thread/resume: thread/resume failed: no rollout found for thread id ` + testkit.CodexID + ` (code -32600)' >&2; exit 1;;
 *fix-1-2*) echo 'temporary startup failure' >&2; exit 1;;
 *fix-1-3*) identity='` + freshID + `'; case "$*" in *'resume -- '*) exit 19;; esac;;
 esac
 `
	script = strings.Replace(script, ` case "$phase_dir" in *fix-*|*review-2-*)`, injected+` case "$phase_dir" in *fix-1-1*|*review-2-*)`, 1)
	if err := os.WriteFile(cfg.Agents.Codex.Executable, []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	if run.Fix.Attempt != 3 || run.Fix.SessionID != freshID || len(run.FixHistory) != 3 || run.Review.SessionID == freshID || api.Creations != 1 || testkit.GitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7") != "2" {
		t.Fatalf("lost recovery: %+v", run)
	}
}

func TestMalformedEarlyReviewerOutputRetriesAfterStopAndRestore(t *testing.T) {
	t.Parallel()

	_, runtime, api, remote, cfg, r := testkit.TmuxPairingFlow(t, "codex", "codex", testkit.DisputedReport)
	cfg.Repositories[0].Reviewer.MaxAttempts = 2
	script := testkit.NativeCodexScript(testkit.ReviewScript(testkit.ApprovedReview))
	prefix := `case "$phase_dir" in *review-1-1*)
 printf tampered > feature.txt
 printf reviewer > reviewer.txt
 printf '%s\n' 'malformed reviewer output'
 while :; do /bin/sleep 0.02; done;;
 esac
 `
	script = strings.Replace(script, ` printf '%s\n' "{\"type\":\"thread.started\",\"thread_id\":\"$identity\"}"`, prefix+` printf '%s\n' "{\"type\":\"thread.started\",\"thread_id\":\"$identity\"}"`, 1)
	if err := os.WriteFile(cfg.Agents.Codex.Executable, []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.Active, workflow.Review)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	firstProcess, err := s.Watch(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WaitFor(t, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		current, err := runtime.Workflow.Get(context.Background(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State == workflow.NeedsAttention {
			t.Fatalf("early malformed output skipped remaining review attempt: %+v", current.Review)
		}
		if current.Review == nil || current.Review.Status != "failed" {
			return false
		}
		if current.Review.Attempt != 1 || current.ReviewRound != 1 || !current.Review.Restored || !current.Review.Contaminated || current.Review.Accepted || current.Review.Report != nil || current.Review.SessionID != "" || !strings.HasPrefix(current.Review.Error, "phase.result_invalid:") {
			t.Fatalf("invalid first attempt: %+v", current.Review)
		}
		return true
	})
	status, err := r.SessionStatus(context.Background(), firstProcess)
	if err != nil || status.State == runner.SessionRunning {
		t.Fatalf("retry left malformed reviewer running: %+v %v", status, err)
	}
	var path string
	if err := runtime.DB.QueryRow("SELECT worktree_path FROM runs WHERE id=?", run.ID).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(path, "feature.txt")); err != nil || string(data) != "implemented\n" {
		t.Fatalf("review source was not restored: %s %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(path, "reviewer.txt")); !os.IsNotExist(err) {
		t.Fatal("reviewer's untracked file survived restoration")
	}
	run = testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	if run.Review.Attempt != 2 || run.ReviewRound != 1 || !run.Review.Accepted || run.Review.SessionID != testkit.ReviewerCodexID || run.Review.SessionID == run.Implementer.SessionID || run.ApprovedSHA != testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7") || api.Creations != 1 {
		t.Fatalf("retry did not approve original pinned head: %+v", run)
	}
}

func TestEarlyReviewerDiscoveryFailuresRespectRetryPolicy(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, Output, Code string
		max, attempt       int
	}{
		{"malformed exhausted", "malformed reviewer output", "phase.result_invalid", 2, 2},
		{"malformed single attempt", "malformed reviewer output", "phase.result_invalid", 1, 1},
		{"invalid identity", `{"type":"thread.started","thread_id":"invalid"}`, "harness.session_identity_invalid", 2, 1},
		{"implementer identity", `{"type":"thread.started","thread_id":"` + testkit.CodexID + `"}`, "harness.session_identity_invalid", 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, runtime, api, _, cfg, r := testkit.TmuxPairingFlow(t, "codex", "codex", testkit.DisputedReport)
			cfg.Repositories[0].Reviewer.MaxAttempts = tc.max
			script := testkit.NativeCodexScript(testkit.ReviewScript(testkit.ApprovedReview))
			prefix := `case "$phase_dir" in *review-*) printf '%s\n' '` + tc.Output + `'; exit 0;; esac
 `
			script = strings.Replace(script, ` printf '%s\n' "{\"type\":\"thread.started\",\"thread_id\":\"$identity\"}"`, prefix+` printf '%s\n' "{\"type\":\"thread.started\",\"thread_id\":\"$identity\"}"`, 1)
			if err := os.WriteFile(cfg.Agents.Codex.Executable, []byte("#!/bin/sh\n"+script), 0700); err != nil {
				t.Fatal(err)
			}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			run := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Review)
			if run.LastErrorCode != tc.Code || run.Review.Attempt != tc.attempt || run.ReviewRound != 1 || !run.Review.Restored || run.Review.Accepted || run.ApprovedSHA != "" || run.Review.SessionID != "" || api.Creations != 1 {
				t.Fatalf("discovery failure bypassed role retry policy: %+v", run)
			}
			for range 2 {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			saved, err := runtime.Workflow.Get(context.Background(), run.ID)
			if err != nil || saved.Review.Attempt != tc.attempt {
				t.Fatalf("attention launched extra attempts: %+v %v", saved, err)
			}
		})
	}
}
