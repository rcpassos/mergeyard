package scheduler_test

import (
	"context"
	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFailingChecksNeverPublishImplementation(t *testing.T) {
	_, rt, api, remote, cfg, r := localFlow(t, successfulScript)
	cfg.Repositories[0].Checks = []string{"printf 'build failed\\n'; exit 7"}
	cfg.Repositories[0].CheckRetries = 0
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.NeedsAttention, workflow.Implement)
	if run.LastErrorCode != "checks.failed" || !strings.Contains(run.LastErrorMessage, "build failed") {
		t.Fatalf("checks diagnostic=%+v", run)
	}
	if api.creations != 0 || gitCommand(t, remote, "branch", "--list", "mergeyard/issue-7") != "" {
		t.Fatal("failing checks published branch")
	}
	if run.Implementer.Attempt != 1 || run.ReviewRound != 0 {
		t.Fatalf("checks spent agent/round budget: %+v", run)
	}
	steps := run.SystemSteps
	if len(steps) != 1 || steps[0].Step != "checks" || steps[0].TargetSHA == "" {
		t.Fatalf("checks metadata=%+v", steps)
	}
	if _, err := s.Watch(context.Background(), run.ID); err == nil {
		t.Fatal("completed checks were watchable")
	}
}

func TestChecksResumeSamePhaseBeforePublicationWithoutSpendingAttempts(t *testing.T) {
	script := `case "$*" in *implement-0-2*)
 printf fixed > feature.txt
 printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"schema_version":3,"criteria":[],"human_action":null,"status":"success","summary":"Fixed checks"}}'
 ;; *) ` + successfulScript + ` ;; esac`
	_, rt, api, remote, cfg, r := localFlow(t, script)
	cfg.Repositories[0].Checks = []string{"/usr/bin/grep -q fixed feature.txt || { printf 'expected fixed\\n'; exit 3; }"}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.Active, workflow.Review)
	if run.Implementer.Attempt != 2 || run.ReviewRound != 1 || api.creations != 1 {
		t.Fatalf("check repair=%+v", run)
	}
	if len(run.SystemSteps) != 2 || run.SystemSteps[0].Status != "failed" || run.SystemSteps[1].Status != "succeeded" {
		t.Fatalf("check results=%+v", run.SystemSteps)
	}
	if gitCommand(t, remote, "show", "mergeyard/issue-7:feature.txt") != "fixed" {
		t.Fatal("published unchecked code")
	}
}

func TestChecksRetryGrantsOneRepairAndRechecksManualCorrectionFirst(t *testing.T) {
	_, rt, api, _, cfg, r := localFlow(t, successfulScript)
	cfg.Repositories[0].Checks = []string{"/usr/bin/grep -q fixed feature.txt || { printf 'expected fixed\\n'; exit 3; }"}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	exhausted := finish(t, s, workflow.NeedsAttention, workflow.Implement)
	if exhausted.Implementer.Attempt != 2 || exhausted.LastErrorCode != "checks.failed" {
		t.Fatalf("unbounded repairs=%+v", exhausted)
	}
	selected, err := s.Retry(context.Background(), exhausted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.Retries) != 1 || selected.Retries[0].GrantedCheck != 1 {
		t.Fatalf("retry grant=%+v", selected.Retries)
	}
	root := rt.Workspace.Root
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	rt, err = app.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close() })
	s, err = scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	exhausted = finish(t, s, workflow.NeedsAttention, workflow.Implement)
	if exhausted.Implementer.Attempt != 3 || len(exhausted.Retries) != 1 {
		t.Fatalf("grant was not exactly one repair: %+v", exhausted)
	}
	tree := setupWorktree(t, rt, exhausted.ID)
	if err := r.WriteFile(context.Background(), tree+"/feature.txt", []byte("fixed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Retry(context.Background(), exhausted.ID); err != nil {
		t.Fatal(err)
	}
	passed := finish(t, s, workflow.Active, workflow.Review)
	if passed.Implementer.Attempt != 3 || api.creations != 1 {
		t.Fatalf("recheck launched agent after manual correction: %+v", passed)
	}
}

func TestChecksGateFixRetriesWithinOneReviewRound(t *testing.T) {
	fix := `case "$*" in *fix-1-1*) printf broken > feature.txt ;; *) printf fixed > feature.txt ;; esac
` + fixedReport
	_, rt, api, remote, cfg, r := pairingFlow(t, "claude", "claude", fix)
	cfg.Repositories[0].Checks = []string{"if /usr/bin/grep -q broken feature.txt; then printf 'fix build failed\\n'; exit 4; fi"}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if run.Fix.Attempt != 2 || run.ReviewRound != 2 || run.Fix.SessionID != run.Implementer.SessionID {
		t.Fatalf("check repair spent rounds or changed conversation: %+v", run)
	}
	if gitCommand(t, remote, "show", "mergeyard/issue-7:feature.txt") != "fixed" {
		t.Fatal("published failed fix")
	}
	if len(run.SystemSteps) != 3 {
		t.Fatalf("missing implement/fix checks: %+v", run.SystemSteps)
	}
}

func TestHandbackChecksFailurePreservesManualWorkAndNeverAutofixes(t *testing.T) {
	_, rt, api, remote, cfg, r := localFlow(t, successfulScript)
	cfg.Repositories[0].Checks = []string{"if /usr/bin/grep -q broken feature.txt; then printf 'manual build failed\\n'; exit 6; fi"}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.Active, workflow.Review)
	if _, err := s.Takeover(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	tree := setupWorktree(t, rt, run.ID)
	if err := r.WriteFile(context.Background(), tree+"/feature.txt", []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Handback(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	failed := finish(t, s, workflow.NeedsAttention, workflow.Review)
	if failed.LastErrorCode != "checks.failed" || failed.Implementer.Attempt != 1 || len(failed.FixHistory) != 0 {
		t.Fatalf("manual edits were auto-fixed: %+v", failed)
	}
	if gitCommand(t, remote, "show", "mergeyard/issue-7:feature.txt") != "implemented" {
		t.Fatal("manual failing commit published")
	}
	if err := r.WriteFile(context.Background(), tree+"/feature.txt", []byte("corrected"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Retry(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	passed := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if passed.Implementer.Attempt != 1 || len(passed.FixHistory) != 0 || gitCommand(t, remote, "show", "mergeyard/issue-7:feature.txt") != "corrected" {
		t.Fatalf("handback retry=%+v", passed)
	}
}

func TestChecksAndCheckRepairSurviveControlPlaneRestart(t *testing.T) {
	for _, mode := range []string{"checks", "repair", "fix"} {
		t.Run(mode, func(t *testing.T) {
			release := filepath.Join(t.TempDir(), "release")
			script := successfulScript
			if mode == "repair" {
				script = `case "$*" in *implement-0-2*) while [ ! -f '` + release + `' ]; do /bin/sleep 0.02; done; printf fixed > feature.txt; printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"schema_version":3,"criteria":[],"human_action":null,"status":"success","summary":"Fixed checks"}}';; *) ` + successfulScript + `;; esac`
			}
			flow := localFlow
			if mode == "fix" {
				script = `case "$*" in *fix-1-1*) printf broken > feature.txt;; *) while [ ! -f '` + release + `' ]; do /bin/sleep 0.02; done; printf fixed > feature.txt;; esac
` + fixedReport
				flow = func(t *testing.T, script string) (*scheduler.Scheduler, *app.Runtime, *fakeGitHub, string, config.Config, runner.Runner) {
					return pairingFlow(t, "claude", "claude", script)
				}
			}
			_, rt, api, _, cfg, r := flow(t, script)
			var githubAPI scheduler.GitHub = api
			if mode == "fix" {
				githubAPI = branchGitHub{api}
			}
			cfg.Repositories[0].Checks = []string{"while [ ! -f '" + release + "' ]; do /bin/sleep 0.02; done"}
			if mode == "repair" {
				cfg.Repositories[0].Checks = []string{"/usr/bin/grep -q fixed feature.txt || exit 4"}
			}
			if mode == "fix" {
				cfg.Repositories[0].Checks = []string{"if /usr/bin/grep -q broken feature.txt; then exit 4; fi"}
			}
			s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: githubAPI, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			var before workflow.Run
			waitForWithin(t, 15*time.Second, func() bool {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				runs, err := s.Runs(context.Background())
				if err != nil || len(runs) != 1 {
					return false
				}
				before = runs[0]
				ref, err := s.Watch(context.Background(), before.ID)
				if err != nil {
					return false
				}
				if mode == "checks" {
					return strings.Contains(ref.Name, "-checks-")
				}
				if mode == "fix" {
					return before.Phase == workflow.Fix && before.Fix != nil && before.Fix.Attempt == 2 && strings.Contains(ref.Name, "-fix-")
				}
				return before.Implementer.Attempt == 2 && strings.Contains(ref.Name, "-implement-")
			})
			ref, err := s.Watch(context.Background(), before.ID)
			if err != nil {
				t.Fatal(err)
			}
			root := rt.Workspace.Root
			if err := rt.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := app.Open(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { reopened.Close() })
			restarted, err := scheduler.New(cfg, schedulerResources(reopened), scheduler.Dependencies{GitHub: githubAPI, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := restarted.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			live, err := restarted.Watch(context.Background(), before.ID)
			if err != nil || live.Name != ref.Name {
				t.Fatalf("restart duplicated owned process: %+v %v", live, err)
			}
			if err := os.WriteFile(release, nil, 0600); err != nil {
				t.Fatal(err)
			}
			done := finish(t, restarted, workflow.Active, workflow.Review)
			want := 1
			if mode == "repair" {
				want = 2
			}
			if mode == "fix" {
				if done.Fix == nil || done.Fix.Attempt != 2 || done.ReviewRound != 2 || len(done.SystemSteps) != 3 {
					t.Fatalf("fix retry restart=%+v", done)
				}
				return
			}
			if done.Implementer.Attempt != want || len(done.SystemSteps) != want {
				t.Fatalf("restart duplicated attempt: %+v", done)
			}
		})
	}
}

func TestUsageLimitDuringCheckRepairRetainsItsAllowance(t *testing.T) {
	script := `case "$*" in *implement-0-2*) exit 1;; *implement-0-3*) printf fixed > feature.txt; printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"schema_version":3,"criteria":[],"human_action":null,"status":"success","summary":"Fixed checks"}}';; *) ` + successfulScript + `;; esac`
	_, rt, api, _, cfg, r := localFlow(t, script)
	cfg.Repositories[0].Checks = []string{"/usr/bin/grep -q fixed feature.txt || exit 3"}
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	fake := &classifiedHarness{HarnessAdapter: harness.NewClaude(cfg.Agents.Claude), outcome: harness.FailureClassification{Kind: harness.TemporaryLimit, ResetAt: now.Add(time.Hour), Source: "fixture"}}
	deps := scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{"claude": fake}}
	s, err := scheduler.New(cfg, schedulerResources(rt), deps)
	if err != nil {
		t.Fatal(err)
	}
	waiting := finish(t, s, workflow.WaitingForHarness, workflow.Implement)
	if waiting.Implementer.Attempt != 2 || waiting.Implementer.CheckRetryOf == "" {
		t.Fatalf("lost check repair provenance: %+v", waiting)
	}
	now = waiting.HarnessWait.ResetAt
	run := finish(t, s, workflow.Active, workflow.Review)
	if run.Implementer.Attempt != 3 || run.Implementer.CheckRetryOf != waiting.Implementer.CheckRetryOf || len(run.SystemSteps) != 2 {
		t.Fatalf("limit spent additional repair: %+v", run)
	}
}

func TestPendingHandbackChecksAreWatchableAndStoppable(t *testing.T) {
	_, rt, api, _, cfg, r := localFlow(t, successfulScript)
	release := filepath.Join(t.TempDir(), "release")
	cfg.Repositories[0].Checks = []string{"if /usr/bin/grep -q manual feature.txt; then while [ ! -f '" + release + "' ]; do /bin/sleep 0.02; done; fi"}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.Active, workflow.Review)
	if _, err := s.Takeover(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	tree := setupWorktree(t, rt, run.ID)
	if err := r.WriteFile(context.Background(), tree+"/feature.txt", []byte("manual"), 0600); err != nil {
		t.Fatal(err)
	}
	handback, err := s.Handback(context.Background(), run.ID)
	if err != nil || handback.State != workflow.Manual || handback.PendingHandback() == nil {
		t.Fatalf("pending checks=%+v err=%v", handback, err)
	}
	ref, err := s.Watch(context.Background(), run.ID)
	if err != nil || !strings.Contains(ref.Name, "-checks-") {
		t.Fatalf("watch=%+v err=%v", ref, err)
	}
	root := rt.Workspace.Root
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := app.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	s, err = scheduler.New(cfg, schedulerResources(reopened), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	live, err := s.Watch(context.Background(), run.ID)
	if err != nil || live.Name != ref.Name {
		t.Fatalf("handback restart=%+v %v", live, err)
	}
	if err := s.Stop(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	stopped, err := s.Runs(context.Background())
	if err != nil || stopped[0].State != workflow.Stopped || stopped[0].PendingHandback() != nil {
		t.Fatalf("stop=%+v err=%v", stopped, err)
	}
	status, err := r.SessionStatus(context.Background(), ref)
	if err != nil || status.State == "running" {
		t.Fatalf("owned checks survived stop: %+v %v", status, err)
	}
}

func TestChecksAlsoRepairCIFixesBeforePush(t *testing.T) {
	fix := `case "$*" in *fix-1-1*) printf broken > feature.txt;; *) printf repaired > feature.txt;; esac
` + ciFixedReport
	_, rt, base, _, cfg, r := localFlow(t, ciFixScript(fix))
	cfg.Repositories[0].Checks = []string{"if /usr/bin/grep -q broken feature.txt; then printf 'CI repair build failed\\n'; exit 4; fi"}
	api := &ciGitHub{fakeGitHub: base, evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "failure"}}}}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: ciBranchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	finish(t, s, workflow.WaitingForCI, workflow.Review)
	finish(t, s, workflow.Active, workflow.Fix)
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
	var checks []workflow.SystemStepSnapshot
	for _, step := range run.SystemSteps {
		if step.Step == "checks" {
			checks = append(checks, step)
		}
	}
	if run.ReviewRound != 2 || run.Fix.Attempt != 2 || len(checks) != 3 || len(run.Fix.Report.Responses) != 0 {
		t.Fatalf("CI check repair=%+v", run)
	}
}

func TestBlockedHarnessCanProbeUsingPendingCheckRepair(t *testing.T) {
	script := `phase_dir=''; previous=''; for arg do if [ "$previous" = '--add-dir' ]; then phase_dir=$arg; fi; previous=$arg; done
if /usr/bin/grep -q '#8' "$phase_dir/input.md"; then exit 1; fi
case "$*" in *implement-0-1*) ` + successfulScript + `;; *) printf fixed > feature.txt; printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"schema_version":3,"criteria":[],"human_action":null,"status":"success","summary":"Fixed checks"}}';; esac`
	_, rt, api, _, cfg, r := localFlow(t, script)
	api.issues["owner/repo"] = append(api.issues["owner/repo"], ready(8))
	cfg.Concurrency = 2
	cfg.Repositories[0].Concurrency = 2
	cfg.Repositories[0].Checks = []string{"/usr/bin/grep -q fixed feature.txt || exit 3"}
	h := &creditHarness{classifiedHarness: &classifiedHarness{HarnessAdapter: harness.NewClaude(cfg.Agents.Claude), outcome: harness.FailureClassification{Kind: harness.CreditsExhausted, Source: "fixture"}}, detection: true}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{"claude": h}})
	if err != nil {
		t.Fatal(err)
	}
	var waiting workflow.Run
	waitForWithin(t, 15*time.Second, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range runs {
			if run.IssueNumber == 7 && run.State == workflow.WaitingForHarness {
				waiting = run
				return true
			}
		}
		return false
	})
	selected, err := s.Retry(context.Background(), waiting.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.CreditProbes) != 1 || selected.CreditProbes[0].Status != "reserved" {
		t.Fatalf("pending check repair did not reserve authorized probe: %+v", selected)
	}
	waitForWithin(t, 15*time.Second, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range runs {
			if run.IssueNumber == 7 && run.Phase == workflow.Review {
				return true
			}
		}
		return false
	})
}

func TestHandbackAfterCheckRepairOpensOrdinaryAttemptBudget(t *testing.T) {
	script := `case "$*" in *implement-0-3*) exit 1;; *implement-0-4*) printf fixed > feature.txt; printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"schema_version":3,"criteria":[],"human_action":null,"status":"success","summary":"Implemented after handback"}}';; *) ` + successfulScript + `;; esac`
	_, rt, api, _, cfg, r := localFlow(t, script)
	cfg.Repositories[0].Implementer.MaxAttempts = 2
	cfg.Repositories[0].Checks = []string{"/usr/bin/grep -q fixed feature.txt || exit 3"}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	failed := finish(t, s, workflow.NeedsAttention, workflow.Implement)
	if _, err := s.Takeover(context.Background(), failed.ID); err != nil {
		t.Fatal(err)
	}
	tree := setupWorktree(t, rt, failed.ID)
	if err := r.WriteFile(context.Background(), tree+"/feature.txt", []byte("fixed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Handback(context.Background(), failed.ID); err != nil {
		t.Fatal(err)
	}
	var done workflow.Run
	waitForWithin(t, 15*time.Second, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		done = runs[0]
		return done.Phase == workflow.Review || done.State == workflow.NeedsAttention
	})
	if done.Phase != workflow.Review || done.Implementer.Attempt != 4 || done.Implementer.CheckRetryOf != "" {
		t.Fatalf("handback lost ordinary budget: %+v", done)
	}
}

func TestHandbackAndFixChecksKeepDistinctOwnedSessionsInSameRound(t *testing.T) {
	_, rt, api, _, cfg, r := localFlow(t, loopScript("printf fixed > feature.txt; "+fixedReport))
	cfg.Repositories[0].Checks = []string{"printf checked"}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.Active, workflow.Review)
	if _, err := s.Takeover(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	tree := setupWorktree(t, rt, run.ID)
	if err := r.WriteFile(context.Background(), tree+"/feature.txt", []byte("manual"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Handback(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	var done workflow.Run
	waitForWithin(t, 15*time.Second, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		done = runs[0]
		return done.State == workflow.WaitingForCI || done.State == workflow.NeedsAttention
	})
	if done.State != workflow.WaitingForCI || len(done.SystemSteps) != 3 || done.SystemSteps[2].Attempt != 2 {
		t.Fatalf("check sessions collided across handback/fix: %+v", done)
	}
}
