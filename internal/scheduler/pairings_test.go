package scheduler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/rcpassos/mergeyard/internal/web"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

const reviewerCodexID = "01a10c5c-8a43-7c52-ba89-5a6d6ec1e22c"

// Adapt the existing Claude fixture reports into native Codex artifacts. The
// fixture insists on the phase schema and complete input on every invocation.
func nativeCodexScript(script string) string {
	return `last_message=''
 schema=''
 previous=''
 for arg do
  if [ "$previous" = '-o' ]; then last_message=$arg; fi
  if [ "$previous" = '--output-schema' ]; then schema=$arg; fi
  previous=$arg
 done
 phase_dir=${schema%/schema.json}
 test -s "$schema" && test -s "$phase_dir/input.md" || exit 9
 /usr/bin/grep -q 'Implement this' "$phase_dir/input.md" || exit 10
 identity='` + codexID + `'
 case "$phase_dir" in *review-*) identity='` + reviewerCodexID + `'; /usr/bin/grep -q 'findings' "$schema" || exit 11;; *fix-*) /usr/bin/grep -q 'responses' "$schema" || exit 12;; esac
 expected_model=implement-model
 expected_effort=medium
 expected_skill=implement
 case "$phase_dir" in *review-*) expected_model=review-model; expected_effort=low; expected_skill=review;; esac
 case "$*" in *"--model $expected_model "*) ;; *) exit 15;; esac
 case "$*" in *"model_reasoning_effort=\"$expected_effort\""*) ;; *) exit 16;; esac
 case "$*" in *"\$$expected_skill Read "*) ;; *) exit 17;; esac
 case "$*" in *'-s workspace-write -c approval_policy="never" -c sandbox_workspace_write.network_access=true'*) ;; *) exit 18;; esac
 case "$phase_dir" in *fix-*|*review-2-*) case "$*" in *"resume -- $identity "*) ;; *) exit 14;; esac;; esac
 case "$*" in *'resume -- '*) case "$*" in *"resume -- $identity "*) ;; *) exit 13;; esac;; esac
 printf '%s\n' "{\"type\":\"thread.started\",\"thread_id\":\"$identity\"}"
 (
 ` + script + `
 ) > "$phase_dir/fixture-report"
 status=$?
 if [ "$status" != 0 ]; then exit "$status"; fi
 # Fixture output is one compact result record with the structured object last.
 sed 's/^.*"structured_output"://; s/}$//' "$phase_dir/fixture-report" > "$last_message"
 printf '%s\n' '{"type":"turn.completed"}'
 `
}

func pairingFlow(t *testing.T, implementer, reviewer, fix string) (*scheduler.Scheduler, *app.Runtime, *fakeGitHub, string, config.Config, runner.Runner) {
	t.Helper()
	script := loopScript(fix)
	_, runtime, api, remote, cfg, r := localFlow(t, script)
	executable := filepath.Join(t.TempDir(), "fake-codex")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+nativeCodexScript(script)), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Agents.Codex.Executable = executable
	cfg.Repositories[0].Implementer = config.Role{Agent: implementer, Model: "implement-model", Effort: "medium", Skills: []string{"implement"}, MaxAttempts: 1}
	cfg.Repositories[0].Reviewer = config.Role{Agent: reviewer, Model: "review-model", Effort: "low", Skills: []string{"review"}, MaxAttempts: 1}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	return s, runtime, api, remote, cfg, r
}

func TestHarnessPairingsCompleteFixLoop(t *testing.T) {
	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			t.Run(implementer+"/"+reviewer, func(t *testing.T) {
				s, runtime, api, remote, _, _ := pairingFlow(t, implementer, reviewer, `printf fixed > feature.txt; `+fixedReport)
				run := finish(t, s, workflow.WaitingForCI, workflow.Review)
				if run.ReviewRound != 2 || run.Review.Agent != reviewer || run.Fix.Agent != implementer || run.Review.SessionID == run.Fix.SessionID || run.Fix.SessionID != run.Implementer.SessionID {
					t.Fatalf("role ownership: %+v review=%+v fix=%+v", run, run.Review, run.Fix)
				}
				if run.Review.Model != "review-model" || run.Fix.Model != "implement-model" || !run.Review.Accepted || len(run.Fix.Report.Responses) != 2 {
					t.Fatal("lost role settings or reports")
				}
				if api.creations != 1 || gitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7") != "2" {
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

func TestHarnessPairingsNoChangeDispute(t *testing.T) {
	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			t.Run(implementer+"/"+reviewer, func(t *testing.T) {
				s, _, _, remote, _, _ := pairingFlow(t, implementer, reviewer, disputedReport)
				run := finish(t, s, workflow.WaitingForCI, workflow.Review)
				if run.ReviewRound != 2 || !run.Review.Accepted || run.Fix.Report.Responses[0].Resolution != "disputed" || gitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7") != "1" {
					t.Fatalf("no-change dispute failed: %+v", run)
				}
			})
		}
	}
}

func replacePhaseScript(t *testing.T, cfg config.Config, phase workflow.Phase, script string) {
	t.Helper()
	executable, agent := cfg.Agents.Claude.Executable, cfg.Repositories[0].Reviewer.Agent
	if phase == workflow.Fix {
		agent = cfg.Repositories[0].Implementer.Agent
	}
	if agent == "codex" {
		executable, script = cfg.Agents.Codex.Executable, nativeCodexScript(script)
	}
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
}

func waitForPhase(t *testing.T, s *scheduler.Scheduler, phase workflow.Phase) workflow.Run {
	t.Helper()
	var result workflow.Run
	waitFor(t, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != 1 || runs[0].Phase != phase {
			return false
		}
		result = runs[0]
		if phase == workflow.Review {
			return result.Review != nil && result.Review.Status == "running" && result.Review.SessionID != ""
		}
		return result.Fix != nil && result.Fix.Status == "running"
	})
	return result
}

func TestHarnessPairingsStopAndRestart(t *testing.T) {
	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			for _, phase := range []workflow.Phase{workflow.Review, workflow.Fix} {
				t.Run(implementer+"/"+reviewer+"/"+string(phase), func(t *testing.T) {
					gate := filepath.Join(t.TempDir(), "release")
					held := `while [ ! -f '` + gate + `' ]; do /bin/sleep 0.02; done; `
					s, runtime, api, _, cfg, r := pairingFlow(t, implementer, reviewer, `printf fixed > feature.txt; `+fixedReport)
					if phase == workflow.Review {
						replacePhaseScript(t, cfg, phase, `case "$*" in *review-1-*) `+held+changesReview+`;; *review-*) `+approvedReview+`;; *fix-*) printf fixed > feature.txt; `+fixedReport+`;; *) `+successfulScript+`;; esac`)
					} else {
						replacePhaseScript(t, cfg, phase, loopScript(held+`printf fixed > feature.txt; `+fixedReport))
					}
					run := waitForPhase(t, s, phase)
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
						server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331"+path, nil))
						identity, model := run.Review.SessionID, "review-model"
						if phase == workflow.Fix {
							identity, model = run.Fix.SessionID, "implement-model"
						}
						if response.Code != 200 || !strings.Contains(response.Body.String(), identity) || !strings.Contains(response.Body.String(), model) {
							t.Fatalf("missing settings: %s", response.Body.String())
						}
					}
					restarted, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
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
					if api.creations != 1 {
						t.Fatal("duplicated PR")
					}
				})
			}
		}
	}
}

func TestHarnessPairingsRejectInvalidReports(t *testing.T) {
	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			for _, phase := range []workflow.Phase{workflow.Review, workflow.Fix} {
				t.Run(implementer+"/"+reviewer+"/"+string(phase), func(t *testing.T) {
					s, _, api, remote, cfg, _ := pairingFlow(t, implementer, reviewer, `printf fixed > feature.txt; `+fixedReport)
					invalid := strings.ReplaceAll(changesReview, `"changes_required"`, `"approved"`)
					script := `case "$*" in *review-*) ` + invalid + `;; *) ` + successfulScript + `;; esac`
					if phase == workflow.Fix {
						script = loopScript(strings.ReplaceAll(fixedReport, `"F1"`, `"unknown"`))
					}
					replacePhaseScript(t, cfg, phase, script)
					run := finish(t, s, workflow.NeedsAttention, phase)
					if run.LastErrorCode != "phase.result_invalid" || run.ReviewRound != 1 || run.ApprovedSHA != "" || api.creations != 1 || gitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7") != "1" {
						t.Fatalf("accepted invalid result: %+v", run)
					}
				})
			}
		}
	}
}

func TestCodexReviewerIdentityBeforeNextPoll(t *testing.T) {
	s, runtime, api, _, cfg, r := pairingFlow(t, "codex", "codex", disputedReport)
	replacePhaseScript(t, cfg, workflow.Review, `case "$*" in *review-*) sleep 30; `+approvedReview+`;; *) `+successfulScript+`;; esac`)
	run := finish(t, s, workflow.Active, workflow.Review)
	cfg.PollInterval = time.Hour
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool {
		saved, err := runtime.Workflow.Get(context.Background(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		return saved.Review != nil && saved.Review.SessionID == reviewerCodexID && saved.Review.Status == "running"
	})
	if err := s.Stop(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCodexReviewRestoresMutationsAndPermitsBuildOutput(t *testing.T) {
	for _, tamper := range []bool{false, true} {
		t.Run(fmt.Sprintf("tamper-%t", tamper), func(t *testing.T) {
			s, runtime, _, _, cfg, _ := pairingFlow(t, "codex", "codex", disputedReport)
			mutation := `/bin/mkdir -p build-output; printf cache > build-output/cache; `
			if tamper {
				mutation += `printf tamper > feature.txt; printf new > reviewer.txt; git add feature.txt; git -c user.name=Reviewer -c user.email=reviewer@example.invalid commit -m 'reviewer commit' >&2; `
			}
			replacePhaseScript(t, cfg, workflow.Review, reviewScript(mutation+approvedReview))
			run := finish(t, s, workflow.Active, workflow.Review)
			var path string
			if err := runtime.DB.QueryRow("SELECT worktree_path FROM runs WHERE id=?", run.ID).Scan(&path); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, ".gitignore"), []byte("build-output/\n"), 0600); err != nil {
				t.Fatal(err)
			}
			gitCommand(t, path, "add", ".gitignore")
			state := workflow.WaitingForCI
			if tamper {
				state = workflow.NeedsAttention
			}
			run = finish(t, s, state, workflow.Review)
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
	for _, phase := range []workflow.Phase{workflow.Review, workflow.Fix} {
		for _, kind := range []string{"schema", "completion", "provider failure"} {
			t.Run(string(phase)+"/"+kind, func(t *testing.T) {
				s, _, api, remote, cfg, _ := pairingFlow(t, "codex", "codex", `printf fixed > feature.txt; `+fixedReport)
				report := fixedReport
				if phase == workflow.Review {
					report = changesReview
				}
				if kind == "schema" {
					report = strings.ReplaceAll(report, `"schema_version":1,`, "")
				}
				if kind == "provider failure" {
					report = `echo 'Unsupported model' >&2; exit 1`
				}
				script := loopScript(report)
				if phase == workflow.Review {
					script = reviewScript(report)
				}
				script = nativeCodexScript(script)
				if kind == "completion" {
					script = strings.ReplaceAll(script, `printf '%s\n' '{"type":"turn.completed"}'`, `case "$phase_dir" in *`+string(phase)+`-*) ;; *) printf '%s\n' '{"type":"turn.completed"}';; esac`)
				}
				if err := os.WriteFile(cfg.Agents.Codex.Executable, []byte("#!/bin/sh\n"+script), 0700); err != nil {
					t.Fatal(err)
				}
				run := finish(t, s, workflow.NeedsAttention, phase)
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
				if run.LastErrorCode != code || run.ApprovedSHA != "" || run.ReviewRound != 1 || api.creations != 1 || gitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7") != "1" {
					t.Fatalf("native contract: %+v", run)
				}
			})
		}
	}
}

func TestReviewStartupFailureHonorsRoleAttemptLimit(t *testing.T) {
	for _, reviewer := range []string{"claude", "codex"} {
		t.Run(reviewer, func(t *testing.T) {
			_, runtime, api, _, cfg, r := pairingFlow(t, "claude", reviewer, disputedReport)
			cfg.Repositories[0].Reviewer.MaxAttempts = 2
			launches := 0
			failed := failReviewLaunch{Runner: r, observe: func() { launches++ }}
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: failed})
			if err != nil {
				t.Fatal(err)
			}
			run := finish(t, s, workflow.NeedsAttention, workflow.Review)
			if launches != 2 || run.Review.Attempt != 2 || run.Review.Status != "failed" || !run.Review.Restored {
				t.Fatalf("unbounded or incomplete launch failure: launches=%d %+v", launches, run.Review)
			}
		})
	}
}

type failReviewLaunch struct {
	runner.Runner
	observe func()
}

func (r failReviewLaunch) StartSession(ctx context.Context, req runner.SessionRequest) (runner.SessionRef, error) {
	if req.Phase == "review" {
		r.observe()
		return runner.SessionRef{}, &fault.Error{Code: "phase.launch_failed", Message: "fixture executable unavailable"}
	}
	return r.Runner.StartSession(ctx, req)
}

func TestCodexVerifiedMissingReviewOrFixStartsFreshBoundedConversation(t *testing.T) {
	const freshID = "01a10c61-253c-7173-a0d7-d82901ccda96"
	for _, phase := range []workflow.Phase{workflow.Review, workflow.Fix} {
		t.Run(string(phase), func(t *testing.T) {
			_, runtime, api, _, cfg, r := pairingFlow(t, "codex", "codex", `printf fixed > feature.txt; `+fixedReport)
			cfg.Repositories[0].Implementer.MaxAttempts = 2
			cfg.Repositories[0].Reviewer.MaxAttempts = 2
			script := nativeCodexScript(loopScript(`printf fixed > feature.txt; ` + fixedReport))
			if phase == workflow.Review {
				// First attempt fails after discovery; retry resumes the captured identity,
				// then a verified missing-rollout signal permits fresh recovery.
				cfg.Repositories[0].Reviewer.MaxAttempts = 3
				injected := `case "$phase_dir" in
     *review-1-1*) printf '%s\n' "{\"type\":\"thread.started\",\"thread_id\":\"$identity\"}"; exit 1;;
     *review-1-2*) echo 'Error: thread/resume: thread/resume failed: no rollout found for thread id ` + reviewerCodexID + ` (code -32600)' >&2; exit 1;;
     *review-1-3*|*review-2-*) identity='` + freshID + `';;
    esac
`
				script = strings.Replace(script, ` case "$phase_dir" in *fix-*|*review-2-*)`, injected+` case "$phase_dir" in *fix-*|*review-2-*)`, 1)
			} else {
				injected := `case "$phase_dir" in
     *fix-1-1*) echo 'Error: thread/resume: thread/resume failed: no rollout found for thread id ` + codexID + ` (code -32600)' >&2; exit 1;;
     *fix-1-2*) identity='` + freshID + `';;
    esac
`
				script = strings.Replace(script, ` case "$phase_dir" in *fix-*|*review-2-*)`, injected+` case "$phase_dir" in *fix-1-1*|*review-2-*)`, 1)
			}
			if err := os.WriteFile(cfg.Agents.Codex.Executable, []byte("#!/bin/sh\n"+script), 0700); err != nil {
				t.Fatal(err)
			}
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			run := finish(t, s, workflow.WaitingForCI, workflow.Review)
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
	const freshID = "01a10c61-253c-7173-a0d7-d82901ccda96"
	_, runtime, api, remote, cfg, r := pairingFlow(t, "codex", "codex", `printf fixed > feature.txt; `+fixedReport)
	cfg.Repositories[0].Implementer.MaxAttempts = 3
	script := nativeCodexScript(loopScript(`printf fixed > feature.txt; ` + fixedReport))
	injected := `case "$phase_dir" in
 *fix-1-1*) echo 'Error: thread/resume: thread/resume failed: no rollout found for thread id ` + codexID + ` (code -32600)' >&2; exit 1;;
 *fix-1-2*) echo 'temporary startup failure' >&2; exit 1;;
 *fix-1-3*) identity='` + freshID + `'; case "$*" in *'resume -- '*) exit 19;; esac;;
 esac
 `
	script = strings.Replace(script, ` case "$phase_dir" in *fix-*|*review-2-*)`, injected+` case "$phase_dir" in *fix-1-1*|*review-2-*)`, 1)
	if err := os.WriteFile(cfg.Agents.Codex.Executable, []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if run.Fix.Attempt != 3 || run.Fix.SessionID != freshID || len(run.FixHistory) != 3 || run.Review.SessionID == freshID || api.creations != 1 || gitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7") != "2" {
		t.Fatalf("lost recovery: %+v", run)
	}
}

func TestMalformedEarlyReviewerOutputRetriesAfterStopAndRestore(t *testing.T) {
	_, runtime, api, remote, cfg, r := pairingFlow(t, "codex", "codex", disputedReport)
	cfg.Repositories[0].Reviewer.MaxAttempts = 2
	script := nativeCodexScript(reviewScript(approvedReview))
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
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.Active, workflow.Review)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	firstProcess, err := s.Watch(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
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
	run = finish(t, s, workflow.WaitingForCI, workflow.Review)
	if run.Review.Attempt != 2 || run.ReviewRound != 1 || !run.Review.Accepted || run.Review.SessionID != reviewerCodexID || run.Review.SessionID == run.Implementer.SessionID || run.ApprovedSHA != gitCommand(t, remote, "rev-parse", "mergeyard/issue-7") || api.creations != 1 {
		t.Fatalf("retry did not approve original pinned head: %+v", run)
	}
}

func TestEarlyReviewerDiscoveryFailuresRespectRetryPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, output, code string
		max, attempt       int
	}{
		{"malformed exhausted", "malformed reviewer output", "phase.result_invalid", 2, 2},
		{"malformed single attempt", "malformed reviewer output", "phase.result_invalid", 1, 1},
		{"invalid identity", `{"type":"thread.started","thread_id":"invalid"}`, "harness.session_identity_invalid", 2, 1},
		{"implementer identity", `{"type":"thread.started","thread_id":"` + codexID + `"}`, "harness.session_identity_invalid", 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, runtime, api, _, cfg, r := pairingFlow(t, "codex", "codex", disputedReport)
			cfg.Repositories[0].Reviewer.MaxAttempts = tc.max
			script := nativeCodexScript(reviewScript(approvedReview))
			prefix := `case "$phase_dir" in *review-*) printf '%s\n' '` + tc.output + `'; exit 0;; esac
 `
			script = strings.Replace(script, ` printf '%s\n' "{\"type\":\"thread.started\",\"thread_id\":\"$identity\"}"`, prefix+` printf '%s\n' "{\"type\":\"thread.started\",\"thread_id\":\"$identity\"}"`, 1)
			if err := os.WriteFile(cfg.Agents.Codex.Executable, []byte("#!/bin/sh\n"+script), 0700); err != nil {
				t.Fatal(err)
			}
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			run := finish(t, s, workflow.NeedsAttention, workflow.Review)
			if run.LastErrorCode != tc.code || run.Review.Attempt != tc.attempt || run.ReviewRound != 1 || !run.Review.Restored || run.Review.Accepted || run.ApprovedSHA != "" || run.Review.SessionID != "" || api.creations != 1 {
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
