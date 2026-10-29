package integration_test

import (
	"context"
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

func TestBaseConflictRepairsOrRequiresAttentionWithinRoundBound(t *testing.T) {
	t.Parallel()

	for _, max := range []int{1, 2} {
		t.Run(fmt.Sprint(max), func(t *testing.T) {
			t.Parallel()

			_, rt, base, remote, cfg, r := testkit.TmuxFlow(t, testkit.CiFixScript(`printf resolved > README.md; `+testkit.CiFixedReport))
			// Both branches edit the same originally tracked file.
			data, err := os.ReadFile(cfg.Agents.Claude.Executable)
			if err != nil {
				t.Fatal(err)
			}
			script := strings.Replace(string(data), "'implemented' > feature.txt", "'implemented' > README.md", 1)
			if err := os.WriteFile(cfg.Agents.Claude.Executable, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			cfg.MaxRounds = max
			api := testkit.PassingBaseAPI(base)
			deps := scheduler.Dependencies{GitHub: testkit.CiBranchGitHub{CiGitHub: api}, Runner: r}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
			if err != nil {
				t.Fatal(err)
			}
			run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
			testkit.AdvanceBase(t, remote, "README.md", "conflicting base\n")
			if max == 1 {
				failed := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Review)
				if failed.LastErrorCode != "git.base_conflict" || failed.ReviewRound != 1 || failed.CI.Base.Conflicts[0] != "README.md" || len(failed.FixHistory) != 0 || api.ReadyCalls != 0 {
					t.Fatalf("unbounded conflict: %+v", failed)
				}
				if _, err := s.Retry(context.Background(), run.ID); err != nil {
					t.Fatal(err)
				}
			}
			ready := testkit.Finish(t, s, workflow.ReadyToMerge, workflow.Review)
			if ready.ReviewRound != 2 || len(ready.FixHistory) != 1 || ready.FixHistory[0].CI.RepairCause != "git.base_conflict" || ready.CI.Base.Status != "passed" || api.ReadyCalls != 1 {
				t.Fatalf("repair=%+v", ready)
			}
			input, err := os.ReadFile(filepath.Join(rt.Workspace.Root, "runs", ready.ID, "phases", "fix-1-1", "input.md"))
			if err != nil || !strings.Contains(string(input), "README.md") || !strings.Contains(string(input), "git.base_conflict") {
				t.Fatalf("fix lacks conflict input: %s %v", input, err)
			}
		})
	}
}

func TestBaseVerificationRunsOnCombinedTreeAndCleansUp(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"explicit", "checks fallback", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			_, rt, base, remote, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
			command := `if [ -f base.txt ]; then test -f feature.txt; printf combined-tree; fi`
			switch mode {
			case "explicit":
				cfg.Repositories[0].VerifyCommands = []string{command}
			case "checks fallback":
				cfg.Repositories[0].Checks = []string{command}
			case "disabled":
				cfg.Repositories[0].Checks = []string{command}
				cfg.Repositories[0].VerifyCommands = []string{}
			}
			api := testkit.PassingBaseAPI(base)
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
			head := run.ApprovedSHA
			baseSHA := testkit.AdvanceBase(t, remote, "base.txt", "new base\n")
			ready := testkit.Finish(t, s, workflow.ReadyToMerge, workflow.Review)
			v := ready.CI.Base
			if v.BaseSHA != baseSHA || v.HeadSHA != head || v.Status != "passed" || !v.Cleaned {
				t.Fatalf("gate=%+v", v)
			}
			tree := testkit.SetupWorktree(t, rt, run.ID)
			if got := testkit.GitCommand(t, tree, "rev-parse", "HEAD"); got != head {
				t.Fatal("verification changed run head")
			}
			if _, err := os.Stat(filepath.Join(tree, "base.txt")); !os.IsNotExist(err) {
				t.Fatal("verification changed run files")
			}
			if got := testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7"); got != head {
				t.Fatal("verification pushed anything")
			}
			if mode == "disabled" {
				for _, step := range ready.SystemSteps {
					if step.Step == "verify" {
						t.Fatal("disabled verification executed")
					}
				}
			} else {
				if !strings.Contains(v.Output, "combined-tree") {
					t.Fatalf("did not check combined tree: %+v", v)
				}
				if _, err := os.Stat(v.ScratchPath); !os.IsNotExist(err) {
					t.Fatal("scratch retained")
				}
				if strings.Contains(testkit.GitCommand(t, tree, "worktree", "list", "--porcelain"), v.ScratchPath) {
					t.Fatal("scratch metadata retained")
				}
			}
		})
	}
}

func TestBaseVerificationFailureUsesCIRepairBound(t *testing.T) {
	t.Parallel()

	for _, max := range []int{1, 2} {
		t.Run(fmt.Sprint(max), func(t *testing.T) {
			t.Parallel()

			_, rt, base, _, cfg, r := testkit.TmuxFlow(t, testkit.CiFixScript(`printf repaired > feature.txt; `+testkit.CiFixedReport))
			cfg.MaxRounds = max
			cfg.Repositories[0].VerifyCommands = []string{`if /usr/bin/grep -q implemented feature.txt; then printf base-build-failed; exit 7; fi`}
			api := testkit.PassingBaseAPI(base)
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: testkit.CiBranchGitHub{CiGitHub: api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
			if max == 1 {
				failed := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Review)
				if failed.LastErrorCode != "git.base_verification_failed" || failed.CI.Base.Status != "failed" || !failed.CI.Base.Cleaned || !strings.Contains(failed.LastErrorMessage, "base-build-failed") || api.ReadyCalls != 0 {
					t.Fatalf("failure=%+v", failed)
				}
				return
			}
			ready := testkit.Finish(t, s, workflow.ReadyToMerge, workflow.Review)
			if ready.ReviewRound != 2 || len(ready.FixHistory) != 1 || ready.FixHistory[0].CI.RepairCause != "git.base_verification_failed" || !strings.Contains(ready.FixHistory[0].CI.Base.Output, "base-build-failed") {
				t.Fatalf("repair=%+v", ready)
			}
		})
	}
}

func TestReadyBaseWarningsAreReadOnlyAndReachDashboard(t *testing.T) {
	t.Parallel()

	_, rt, base, remote, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	api := testkit.PassingBaseAPI(base)
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	ready := testkit.Finish(t, s, workflow.ReadyToMerge, workflow.Review)
	for _, tc := range []struct{ name, content, warning string }{
		{"base.txt", "advance\n", "stale vs base"},
		{"feature.txt", "conflicting\n", "conflicts with base"},
	} {
		testkit.AdvanceBase(t, remote, tc.name, tc.content)
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		saved, err := rt.Workflow.Get(context.Background(), ready.ID)
		if err != nil {
			t.Fatal(err)
		}
		if saved.State != workflow.ReadyToMerge || saved.ApprovedSHA != ready.ApprovedSHA || saved.ReviewRound != ready.ReviewRound || len(saved.SystemSteps) != len(ready.SystemSteps) || len(saved.FixHistory) != 0 || !strings.Contains(saved.CI.BaseWarning, tc.warning) {
			t.Fatalf("warning re-ran phases: %+v", saved)
		}
		server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/", "/runs/" + ready.ID} {
			res := httptest.NewRecorder()
			server.ServeHTTP(res, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331"+path, nil))
			if res.Code != 200 || !strings.Contains(res.Body.String(), tc.warning) {
				t.Fatalf("dashboard %s lacks %s", path, tc.warning)
			}
		}
	}
}

func TestBaseVerificationSurvivesRestartAndBaseMovement(t *testing.T) {
	t.Parallel()

	_, rt, base, remote, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	release := filepath.Join(t.TempDir(), "release")
	count := filepath.Join(t.TempDir(), "count")
	cfg.Repositories[0].VerifyCommands = []string{`printf run >> '` + count + `'; while [ ! -f '` + release + `' ]; do /bin/sleep 0.02; done; test -f feature.txt`}
	api := testkit.PassingBaseAPI(base)
	deps := scheduler.Dependencies{GitHub: api, Runner: r}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
	if err != nil {
		t.Fatal(err)
	}
	testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	deadline := time.Now().Add(5 * time.Second)
	var running workflow.Run
	for time.Now().Before(deadline) {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		running = runs[0]
		if len(running.SystemSteps) == 1 {
			break
		}
	}
	if len(running.SystemSteps) != 1 || running.CI.Base.Cleaned {
		t.Fatal("verification did not start")
	}
	step := running.SystemSteps[0]
	s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
	if err != nil {
		t.Fatal(err)
	}
	baseSHA := testkit.AdvanceBase(t, remote, "base.txt", "moved during verification\n")
	report, err := s.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range report.Findings {
		if finding.Code == "reconcile.orphaned_worktree" || finding.Code == "reconcile.orphaned_session" {
			t.Fatalf("lost scratch ownership: %+v", finding)
		}
	}
	if _, err := os.Stat(running.CI.Base.ScratchPath); err != nil {
		t.Fatal("removed live scratch")
	}
	if err := os.WriteFile(release, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	testkit.AwaitSetupSession(t, r, runner.SessionRef{Name: step.ProcessSession, PhaseDir: step.PhaseDir})
	// Restart again after process exit, before the scheduler consumes its result.
	s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
	if err != nil {
		t.Fatal(err)
	}
	ready := testkit.Finish(t, s, workflow.ReadyToMerge, workflow.Review)
	if ready.CI.Base.BaseSHA != baseSHA || len(ready.SystemSteps) != 2 || ready.SystemSteps[0].Status != "succeeded" || ready.SystemSteps[1].Status != "succeeded" {
		t.Fatalf("restart=%+v", ready.SystemSteps)
	}
	data, err := os.ReadFile(count)
	if err != nil || string(data) != "runrun" {
		t.Fatalf("duplicated commands: %s %v", data, err)
	}
	for _, path := range []string{running.CI.Base.ScratchPath, ready.CI.Base.ScratchPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("scratch left: %s %v", path, err)
		}
	}
}

func TestBaseScratchCleanupResumesWithoutReplayingCompletedCommands(t *testing.T) {
	t.Parallel()

	_, rt, base, _, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	count := filepath.Join(t.TempDir(), "count")
	cfg.Repositories[0].VerifyCommands = []string{`printf run >> '` + count + `'`}
	api := testkit.PassingBaseAPI(base)
	deps := scheduler.Dependencies{GitHub: api, Runner: r}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	running, err := rt.Workflow.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	step := running.SystemSteps[0]
	testkit.AwaitSetupSession(t, r, runner.SessionRef{Name: step.ProcessSession, PhaseDir: step.PhaseDir})
	// Fail only the cleanup acknowledgement, after Git removed the scratch.
	if _, err := rt.DB.Exec(`CREATE TRIGGER reject_base_cleanup BEFORE UPDATE ON ci_waits WHEN NEW.snapshot_json LIKE '%"cleaned":true%' BEGIN SELECT RAISE(FAIL,'cleanup crash'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background()); err == nil {
		t.Fatal("cleanup acknowledgement should fail")
	}
	saved, err := rt.Workflow.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.CI.Base.Status != "passed" || saved.CI.Base.Cleaned {
		t.Fatalf("lost completion: %+v", saved.CI.Base)
	}
	if _, err := os.Stat(saved.CI.Base.ScratchPath); !os.IsNotExist(err) {
		t.Fatal("scratch was not removed before crash")
	}
	if _, err := rt.DB.Exec("DROP TRIGGER reject_base_cleanup"); err != nil {
		t.Fatal(err)
	}
	s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
	if err != nil {
		t.Fatal(err)
	}
	ready := testkit.Finish(t, s, workflow.ReadyToMerge, workflow.Review)
	data, err := os.ReadFile(count)
	if err != nil || string(data) != "run" || len(ready.SystemSteps) != 1 || !ready.CI.Base.Cleaned {
		t.Fatalf("cleanup replayed commands: %q steps=%+v err=%v", data, ready.SystemSteps, err)
	}
}

func TestStopDuringBaseVerificationStopsProcessesAndReconcilesScratch(t *testing.T) {
	t.Parallel()

	_, rt, base, _, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	cfg.Repositories[0].VerifyCommands = []string{"while :; do /bin/sleep 1; done"}
	api := testkit.PassingBaseAPI(base)
	deps := scheduler.Dependencies{GitHub: api, Runner: r}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	running, err := rt.Workflow.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	stopped, err := rt.Workflow.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != workflow.Stopped || !stopped.CI.Base.Cleaned {
		t.Fatalf("stop lost cleanup: %+v", stopped)
	}
	if _, err := os.Stat(running.CI.Base.ScratchPath); !os.IsNotExist(err) {
		t.Fatal("stopped scratch retained")
	}
	if _, err := os.Stat(testkit.SetupWorktree(t, rt, run.ID)); err != nil {
		t.Fatal("run worktree lost")
	}
}

func TestBaseVerificationTimeoutRetainsCommandEvidenceAndCleansScratch(t *testing.T) {
	t.Parallel()

	_, rt, base, _, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	cfg.MaxRounds = 1
	cfg.Repositories[0].CommandTimeout = 200 * time.Millisecond
	cfg.Repositories[0].VerifyCommands = []string{"printf verification-started; while :; do /bin/sleep 1; done", "printf should-not-run"}
	api := testkit.PassingBaseAPI(base)
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	failed := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Review)
	if failed.LastErrorCode != "git.base_verification_failed" || !failed.CI.Base.Cleaned || !strings.Contains(failed.CI.Base.Error, cfg.Repositories[0].VerifyCommands[0]) || api.ReadyCalls != 0 {
		t.Fatalf("timeout=%+v", failed.CI)
	}
	step := failed.SystemSteps[0]
	if !step.TimedOut || len(step.Results) != 2 || step.Results[0].Status != "fail" || step.Results[1].Status != "skipped" || !strings.Contains(step.Results[0].Output, "verification-started") {
		t.Fatalf("timeout evidence=%+v", step)
	}
}

func TestBaseConflictRepairCanPreserveFilesWhileMergingBaseAncestry(t *testing.T) {
	t.Parallel()

	_, rt, base, remote, cfg, r := testkit.TmuxFlow(t, testkit.CiFixScript(`printf '%s\n' implemented > README.md; `+testkit.CiFixedReport))
	data, err := os.ReadFile(cfg.Agents.Claude.Executable)
	if err != nil {
		t.Fatal(err)
	}
	script := strings.Replace(string(data), "'implemented' > feature.txt", "'implemented' > README.md", 1)
	if err := os.WriteFile(cfg.Agents.Claude.Executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.MaxRounds = 2
	api := testkit.PassingBaseAPI(base)
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: testkit.CiBranchGitHub{CiGitHub: api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	approved := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	baseSHA := testkit.AdvanceBase(t, remote, "README.md", "conflicting base\n")
	ready := testkit.Finish(t, s, workflow.ReadyToMerge, workflow.Review)
	if ready.ReviewRound != 2 || ready.ApprovedSHA == approved.ApprovedSHA {
		t.Fatalf("merge ancestry was not reviewed: %+v", ready)
	}
	if testkit.GitCommand(t, remote, "rev-parse", ready.ApprovedSHA+"^{tree}") != testkit.GitCommand(t, remote, "rev-parse", approved.ApprovedSHA+"^{tree}") {
		t.Fatal("fixture did not preserve the tree")
	}
	testkit.GitCommand(t, remote, "merge-base", "--is-ancestor", baseSHA, ready.ApprovedSHA)
}

func TestBaseRepairCannotStartIfPRReadCrossesDeadline(t *testing.T) {
	t.Parallel()

	_, rt, base, remote, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	reads := 0
	delayed := false
	api := testkit.PassingBaseAPI(base)
	boundary := testkit.CiDeadlineGitHub{CiGitHub: api, BeforeRead: func() {
		if delayed {
			reads++
			if reads == 3 {
				now = now.Add(time.Hour)
			}
		}
	}}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: boundary, Runner: r, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	testkit.AdvanceBase(t, remote, "feature.txt", "conflicting base\n")
	delayed = true
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, err := rt.Workflow.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.State != workflow.NeedsAttention || saved.LastErrorCode != "ci.wait_timeout" || len(saved.FixHistory) != 0 || saved.ApprovedSHA != run.ApprovedSHA {
		t.Fatalf("late repair authorized: %+v", saved)
	}
}
