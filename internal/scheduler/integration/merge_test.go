package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestEarlyMergeWaitsForProcessExitBeforeReleasingCapacity(t *testing.T) {

	_, runtime, base, remote, cfg, realRunner := testkit.TmuxFlow(t, testkit.ReviewScript(`printf tampered > feature.txt; /bin/sleep 60`))
	api := &testkit.MergeGitHub{CiGitHub: &testkit.CiGitHub{FakeGitHub: base}, ApplyClose: true}
	r := &testkit.PendingStopRunner{Runner: realRunner, Pending: true}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.Active, workflow.Review)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	ref, err := s.Watch(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	head := testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7")
	base.Prs["mergeyard/issue-7"].State, base.Prs["mergeyard/issue-7"].Merged = github.Closed, true
	base.Issues["owner/repo"] = append(base.Issues["owner/repo"], testkit.Ready(8))
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(context.Background())
	if len(runs) != 1 || runs[0].State != workflow.Active || runs[0].Merge == nil || runs[0].Merge.ProcessExited || !runs[0].Merge.Early {
		t.Fatalf("released before exit: %+v", runs)
	}
	server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	server.ServeHTTP(res, testkit.DashboardRequest(t, server, "GET", "http://127.0.0.1:7331/runs/"+run.ID, nil))
	if !strings.Contains(res.Body.String(), "Stop owned phase and verify exit") || strings.Contains(res.Body.String(), "Retry Stop") {
		t.Fatal("pending merge shows ordinary stop instructions")
	}
	// Failed stop survives a scheduler restart and Stop cannot change merge intent to STOPPED.
	root := runtime.Workspace.Root
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := app.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	r.Pending = false
	s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(reopened), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	saved, _ := reopened.Workflow.Get(context.Background(), run.ID)
	status, err := realRunner.SessionStatus(context.Background(), ref)
	if err != nil || status.State == runner.SessionRunning || saved.State != workflow.Completed || saved.Merge.Pending() {
		t.Fatalf("merge stop: %+v %+v %v", saved, saved.Merge, err)
	}
	if testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7") != head {
		t.Fatal("pushed after merge")
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, _ = s.Runs(context.Background())
	if len(runs) != 2 {
		t.Fatal("exit did not release capacity")
	}
}

func TestDirtyMergeCleanupPreservesWorkAndDispatchesAnotherIssue(t *testing.T) {

	_, runtime, base, remote, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	api := &testkit.MergeGitHub{CiGitHub: &testkit.CiGitHub{FakeGitHub: base}, ApplyClose: true}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	var path string
	if err := runtime.DB.QueryRow("SELECT worktree_path FROM runs WHERE id=?", run.ID).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "human.txt"), []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	base.Prs["mergeyard/issue-7"].State, base.Prs["mergeyard/issue-7"].Merged = github.Closed, true
	base.Issues["owner/repo"] = append(base.Issues["owner/repo"], testkit.Ready(8))
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(context.Background())
	saved, _ := runtime.Workflow.Get(context.Background(), run.ID)
	if len(runs) != 2 || saved.State != workflow.Completed || !saved.Merge.Pending() || saved.Merge.WorktreeRemoved || !strings.Contains(saved.Merge.Error, "preserved") {
		t.Fatalf("dirty merge: %+v %+v", runs, saved.Merge)
	}
	data, err := os.ReadFile(filepath.Join(path, "human.txt"))
	if err != nil || string(data) != "keep me" {
		t.Fatal("lost dirty work")
	}
	// Retry after a real database reopen, with the control plane paused.
	root := runtime.Workspace.Root
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := app.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(reopened), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(path, "human.txt")); err != nil {
		t.Fatal(err)
	}
	s.Pause(context.Background(), true)
	for range 2 {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	saved, _ = reopened.Workflow.Get(context.Background(), run.ID)
	if saved.Merge.Pending() || saved.Merge.Error != "" {
		t.Fatalf("retry incomplete: %+v", saved.Merge)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("tree not removed")
	}
	if testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7") == "" {
		t.Fatal("remote branch removed")
	}
}

func TestMergeMaintenanceRestartsAtEachBoundary(t *testing.T) {

	for _, boundary := range []string{"merge_observed", "process_exited", "coding_completed", "ready_removed", "running_removed", "attention_removed", "issue_closed", "review_restored", "worktree_started", "worktree_removed", "branch_deleted"} {
		t.Run(boundary, func(t *testing.T) {

			_, runtime, base, _, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
			api := &testkit.MergeGitHub{CiGitHub: &testkit.CiGitHub{FakeGitHub: base}, ApplyClose: true}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
			pr := base.Prs["mergeyard/issue-7"]
			pr.State, pr.Merged = github.Closed, true
			condition := fmt.Sprintf("NEW.type='merge.cleanup_updated' AND NEW.payload_json LIKE '%%\"%s\":true%%'", boundary)
			if boundary == "merge_observed" {
				condition = "NEW.type='pr.merge_observed'"
			}
			if boundary == "coding_completed" {
				condition = "NEW.type='run.completed'"
			}
			if _, err := runtime.DB.Exec("CREATE TRIGGER reject_merge BEFORE INSERT ON events WHEN " + condition + " BEGIN SELECT RAISE(FAIL,'interrupted acknowledgement'); END"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Reconcile(context.Background()); err == nil {
				t.Fatal("boundary did not reject acknowledgement")
			}
			root := runtime.Workspace.Root
			if err := runtime.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := app.Open(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if _, err := reopened.DB.Exec("DROP TRIGGER reject_merge"); err != nil {
				t.Fatal(err)
			}
			s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(reopened), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			s.Pause(context.Background(), true)
			for range 2 {
				if _, err := s.Reconcile(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			saved, err := reopened.Workflow.Get(context.Background(), run.ID)
			if err != nil || saved.State != workflow.Completed || saved.Merge == nil || saved.Merge.Pending() || saved.Merge.Error != "" {
				t.Fatalf("restart: %+v merge=%+v %v", saved, saved.Merge, err)
			}
			var path, branch string
			basePath := filepath.Join(root, "repos", "owner-repo", "base")
			if err := reopened.DB.QueryRow("SELECT worktree_path,branch FROM runs WHERE id=?", run.ID).Scan(&path, &branch); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("worktree retained")
			}
			if got := testkit.GitCommand(t, basePath, "branch", "--list", branch); got != "" {
				t.Fatal("local branch retained")
			}
			var attempts int
			if err := reopened.DB.QueryRow("SELECT count(*) FROM phase_attempts WHERE run_id=?", run.ID).Scan(&attempts); err != nil || attempts != 2 {
				t.Fatal("restart relaunched work")
			}
		})
	}
}

func TestUnexplainedMissingMergeWorktreeRequiresAttention(t *testing.T) {

	s, runtime, api, _, _, _ := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	var path string
	runtime.DB.QueryRow("SELECT worktree_path FROM runs WHERE id=?", run.ID).Scan(&path)
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	api.Prs["mergeyard/issue-7"].State, api.Prs["mergeyard/issue-7"].Merged = github.Closed, true
	api.Issues["owner/repo"][0].State = github.Closed
	for range 2 {
		if _, err := s.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	saved, _ := runtime.Workflow.Get(context.Background(), run.ID)
	if saved.State != workflow.Completed || saved.Merge.WorktreeStarted || saved.Merge.WorktreeRemoved || saved.Merge.BranchDeleted || saved.Merge.Error == "" {
		t.Fatalf("unexplained loss treated as cleanup: %+v", saved.Merge)
	}
}

func TestMergeStopsChildrenAfterWrapperExitedWhileOffline(t *testing.T) {

	// The reviewer starts a child that outlives its command and ignores soft
	// signals. exit.json and disappearance of tmux alone cannot prove exit.
	script := testkit.ReviewScript(`case "$*" in *review-*) (trap '' INT TERM HUP; while :; do /bin/sleep 0.02; done) & printf '%s' "$!" > child.pid;; esac
` + testkit.ApprovedReview)
	s, runtime, api, _, cfg, r := testkit.TmuxFlow(t, script)
	run := testkit.Finish(t, s, workflow.Active, workflow.Review)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	var path string
	runtime.DB.QueryRow("SELECT worktree_path FROM runs WHERE id=?", run.ID).Scan(&path)
	ref, err := s.Watch(context.Background(), run.ID)
	// A quick exit may already make Watch unavailable; use its recorded phase dir.
	if err != nil {
		ref = runner.SessionRef{PhaseDir: filepath.Join(runtime.Workspace.Root, "runs", run.ID, "phases", "review-1-1")}
	}
	testkit.WaitFor(t, func() bool { _, err := os.Stat(filepath.Join(ref.PhaseDir, "exit.json")); return err == nil })
	data, err := os.ReadFile(filepath.Join(path, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatal("test child did not survive wrapper")
	}
	root := runtime.Workspace.Root
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := app.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	api.Prs["mergeyard/issue-7"].State, api.Prs["mergeyard/issue-7"].Merged = github.Closed, true
	api.Issues["owner/repo"][0].State = github.Closed
	s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(reopened), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, _ := reopened.Workflow.Get(context.Background(), run.ID)
	if saved.State != workflow.Completed {
		t.Fatalf("merge did not finish: %+v", saved.Merge)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("completed with live owned child: %v", err)
	}
}

func TestMergeRecoversRunningAttemptThatNeverLaunched(t *testing.T) {

	_, runtime, api, _, cfg, realRunner := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	r := testkit.UnstartedReviewRunner{Runner: realRunner}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.Active, workflow.Review)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	root := runtime.Workspace.Root
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := app.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	api.Prs["mergeyard/issue-7"].State, api.Prs["mergeyard/issue-7"].Merged = github.Closed, true
	api.Issues["owner/repo"][0].State = github.Closed
	s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(reopened), scheduler.Dependencies{GitHub: api, Runner: realRunner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, _ := reopened.Workflow.Get(context.Background(), run.ID)
	if saved.State != workflow.Completed || !saved.Merge.ProcessExited || saved.Merge.Pending() {
		t.Fatalf("unstarted attempt blocked merge: %+v", saved.Merge)
	}
}

func TestEarlyMergePreservesLocallyCommittedUnpublishedFix(t *testing.T) {

	s, runtime, api, remote, cfg, r := testkit.TmuxFlow(t, testkit.LoopScript(`printf fixed > feature.txt; `+testkit.FixedReport))
	run := testkit.Finish(t, s, workflow.Active, workflow.Fix)
	testkit.AdvanceFixUntil(t, s, runtime, run.ID, func(v workflow.Run) bool { return v.Fix != nil && v.Fix.CommitSHA != "" && !v.Fix.Pushed })
	saved, _ := runtime.Workflow.Get(context.Background(), run.ID)
	unpublished := saved.Fix.CommitSHA
	published := testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7")
	if unpublished == published {
		t.Fatal("fixture already pushed fix")
	}
	var path string
	runtime.DB.QueryRow("SELECT worktree_path FROM runs WHERE id=?", run.ID).Scan(&path)
	api.Prs["mergeyard/issue-7"].State, api.Prs["mergeyard/issue-7"].Merged = github.Closed, true
	api.Issues["owner/repo"][0].State = github.Closed
	for range 2 {
		if _, err := s.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	saved, _ = runtime.Workflow.Get(context.Background(), run.ID)
	if saved.State != workflow.Completed || saved.Merge == nil || !saved.Merge.Pending() || saved.Merge.WorktreeRemoved || saved.Merge.BranchDeleted {
		t.Fatalf("unpublished fix cleanup: %+v", saved.Merge)
	}
	if testkit.GitCommand(t, path, "rev-parse", "HEAD") != unpublished || testkit.GitCommand(t, remote, "rev-parse", "mergeyard/issue-7") != published {
		t.Fatal("lost or pushed unpublished commit")
	}
	if data, err := os.ReadFile(filepath.Join(path, "feature.txt")); err != nil || string(data) != "fixed" {
		t.Fatal("lost unpublished source")
	}
	root := runtime.Workspace.Root
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := app.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	next, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(reopened), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := next.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if testkit.GitCommand(t, path, "rev-parse", "HEAD") != unpublished {
		t.Fatal("restart lost unpublished work")
	}
}

func TestFailedPRReadDoesNotBypassPersistedStop(t *testing.T) {
	t.Parallel()

	_, runtime, base, _, cfg, r := testkit.TmuxFlow(t, testkit.ReviewScript(`/bin/sleep 60`))
	api := &testkit.FailedPRReadGitHub{MergeGitHub: &testkit.MergeGitHub{CiGitHub: &testkit.CiGitHub{FakeGitHub: base}, ApplyClose: true}}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.Active, workflow.Review)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	base.Mutate = func(string, string, int, string) error { return fmt.Errorf("label removal unavailable") }
	if err := s.Stop(context.Background(), run.ID); err == nil {
		t.Fatal("fixture stop already completed")
	}
	base.Mutate = nil
	api.ReadError = fmt.Errorf("PR read unavailable")
	api.PartialMerge = true
	root := runtime.Workspace.Root
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := app.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(reopened), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	_, reconcileErr := s.Reconcile(context.Background())
	saved, err := reopened.Workflow.Get(context.Background(), run.ID)
	if err != nil || saved.State != workflow.Stopped {
		t.Fatalf("failed PR read bypassed stop: state=%s reconcile=%v get=%v", saved.State, reconcileErr, err)
	}
}

func TestMergeCleanupWaitsForInteractiveTakeoverExit(t *testing.T) {

	_, runtime, base, _, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	api := &testkit.MergeGitHub{CiGitHub: &testkit.CiGitHub{FakeGitHub: base}, ApplyClose: true}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	if _, err := s.Takeover(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	worktree := testkit.RunWorktree(t, runtime.Workspace.Root, run)
	gate := t.TempDir()
	started, release := filepath.Join(gate, "started"), filepath.Join(gate, "release")
	interactiveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	t.Cleanup(func() { os.WriteFile(release, nil, 0600) })
	done := make(chan error, 1)
	go func() {
		done <- runner.RunInteractive(interactiveCtx, runtime.Workspace.Root, run.ID, runner.ExecRequest{
			Executable: "/bin/sh", Dir: worktree,
			Args: []string{"-c", `printf ready > "$1"; while [ ! -f "$2" ]; do sleep 0.02; done`, "interactive", started, release},
		}, nil, io.Discard, io.Discard)
	}()
	testkit.WaitFor(t, func() bool { _, err := os.Stat(started); return err == nil })
	api.Prs["mergeyard/issue-7"].State, api.Prs["mergeyard/issue-7"].Merged = github.Closed, true
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("interactive child exited before release: %v", err)
	default:
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("merge removed worktree while interactive child was still alive: %v", err)
	}
	saved, err := runtime.Workflow.Get(ctx, run.ID)
	if err != nil || saved.State != workflow.Completed || !saved.Merge.Pending() || saved.Merge.WorktreeRemoved || saved.Merge.BranchDeleted || !saved.Merge.IssueClosed {
		t.Fatalf("interactive merge cleanup: %+v %v", saved, err)
	}
	if testkit.GitCommand(t, worktree, "branch", "--show-current") != "mergeyard/issue-7" {
		t.Fatal("removed interactive branch")
	}

	// Ownership survives a control-plane restart while the same child is alive.
	root := runtime.Workspace.Root
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := app.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	next, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(reopened), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := next.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("restart removed live interactive worktree: %v", err)
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interactive child did not exit")
	}
	for range 2 {
		if _, err := next.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	saved, err = reopened.Workflow.Get(ctx, run.ID)
	if err != nil || saved.Merge.Pending() || !saved.Merge.WorktreeRemoved || !saved.Merge.BranchDeleted || saved.Merge.Error != "" {
		t.Fatalf("cleanup did not resume after interactive exit: %+v %v", saved.Merge, err)
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("merged worktree was not cleaned after exit: %v", err)
	}
}

func TestMergeCleanupRemovesSlowWorktreeBeyondMaintenanceBudget(t *testing.T) {
	_, runtime, base, _, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	api := &testkit.MergeGitHub{CiGitHub: &testkit.CiGitHub{FakeGitHub: base}, ApplyClose: true}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	worktree := testkit.RunWorktree(t, runtime.Workspace.Root, run)
	marker := filepath.Join(t.TempDir(), "removal-started")
	// Removal is slower than the optional GitHub maintenance budget. The
	// marker proves the Git step ran instead of being cancelled at five
	// seconds, as it was when both kinds of cleanup shared one deadline.
	testkit.InstallGitWrapper(t, "worktree remove", `printf started > "`+marker+`"; /bin/sleep 6`)
	api.Prs["mergeyard/issue-7"].State, api.Prs["mergeyard/issue-7"].Merged = github.Closed, true
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, err := runtime.Workflow.Get(context.Background(), run.ID)
	if err != nil || saved.State != workflow.Completed || saved.Merge.Pending() || !saved.Merge.WorktreeRemoved || !saved.Merge.BranchDeleted || saved.Merge.Error != "" {
		t.Fatalf("slow removal: %+v %v", saved.Merge, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("worktree removal was not exercised")
	}
	if api.RemoveBudget <= 0 || api.RemoveBudget > 5*time.Second {
		t.Fatalf("GitHub maintenance budget = %s", api.RemoveBudget)
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatal("worktree retained")
	}
}

func TestMergeCleanupRecoversInterruptedWorktreeRemoval(t *testing.T) {
	for _, linkGone := range []bool{false, true} {
		name := "link survives"
		if linkGone {
			name = "link removed"
		}
		t.Run(name, func(t *testing.T) {
			_, runtime, base, _, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
			api := &testkit.MergeGitHub{CiGitHub: &testkit.CiGitHub{FakeGitHub: base}, ApplyClose: true}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
			worktree := testkit.RunWorktree(t, runtime.Workspace.Root, run)
			control := filepath.Join(t.TempDir(), "interrupt")
			if err := os.WriteFile(control, nil, 0600); err != nil {
				t.Fatal(err)
			}
			// The first removal deletes tracked files and may or may not reach
			// the link, then fails; later removals go straight to the real Git.
			remove := `rm -f "$last/README.md"`
			if linkGone {
				remove += `; rm -f "$last/.git"`
			}
			testkit.InstallGitWrapper(t, "worktree remove", `if [ -f "`+control+`" ]; then
  rm -f "`+control+`"
  for last; do :; done
  `+remove+`
  exit 1
fi`)
			api.Prs["mergeyard/issue-7"].State, api.Prs["mergeyard/issue-7"].Merged = github.Closed, true
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			saved, err := runtime.Workflow.Get(context.Background(), run.ID)
			if err != nil || saved.State != workflow.Completed || !saved.Merge.WorktreeStarted || saved.Merge.WorktreeRemoved || saved.Merge.Error == "" {
				t.Fatalf("interrupted removal not recorded: %+v %v", saved.Merge, err)
			}
			_, linkErr := os.Stat(filepath.Join(worktree, ".git"))
			if linkGone && !os.IsNotExist(linkErr) {
				t.Fatalf("fixture did not remove the worktree link: %v", linkErr)
			}
			if !linkGone && linkErr != nil {
				t.Fatalf("fixture did not keep the worktree link: %v", linkErr)
			}
			if _, err := os.Stat(worktree); err != nil {
				t.Fatalf("fixture removed the whole worktree: %v", err)
			}
			intent := filepath.Join(filepath.Dir(worktree), run.ID+".cleanup")
			if _, err := os.Stat(intent); err != nil {
				t.Fatalf("removal boundary not recorded: %v", err)
			}
			// A later tick finishes the authorized removal without manual steps.
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			saved, err = runtime.Workflow.Get(context.Background(), run.ID)
			if err != nil || saved.Merge.Pending() || !saved.Merge.WorktreeRemoved || !saved.Merge.BranchDeleted || saved.Merge.Error != "" {
				t.Fatalf("interrupted removal did not recover: %+v %v", saved.Merge, err)
			}
			if _, err := os.Stat(worktree); !os.IsNotExist(err) {
				t.Fatal("worktree retained")
			}
			if _, err := os.Stat(intent); !os.IsNotExist(err) {
				t.Fatal("cleanup intent retained")
			}
		})
	}
}

func TestInteractiveResumeCannotLaunchDuringMergeCleanup(t *testing.T) {

	s, runtime, base, _, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	if _, err := s.Takeover(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	worktree := testkit.RunWorktree(t, runtime.Workspace.Root, run)
	held := &testkit.HeldMergeCleanupGit{Manager: testkit.TestGitManager(runtime.Workspace), Started: make(chan struct{}), Release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(held.Release) }) }
	defer release()
	api := &testkit.MergeGitHub{CiGitHub: &testkit.CiGitHub{FakeGitHub: base}, ApplyClose: true}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Git: held, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	api.Prs["mergeyard/issue-7"].State, api.Prs["mergeyard/issue-7"].Merged = github.Closed, true
	done := make(chan error, 1)
	go func() { _, err := s.Reconcile(context.Background()); done <- err }()
	select {
	case <-held.Started:
	case <-time.After(5 * time.Second):
		t.Fatal("merge cleanup did not start")
	}
	started := filepath.Join(t.TempDir(), "interactive-started")
	err = runner.RunInteractive(context.Background(), runtime.Workspace.Root, run.ID, runner.ExecRequest{
		Executable: "/bin/sh", Dir: worktree,
		Args: []string{"-c", `printf started > "$1"`, "interactive", started},
	}, nil, io.Discard, io.Discard)
	var failure *fault.Error
	if !errors.As(err, &failure) || failure.Code != "takeover.interactive_running" {
		t.Fatalf("interactive launch raced cleanup: %v", err)
	}
	if _, err := os.Stat(started); !os.IsNotExist(err) {
		t.Fatalf("interactive process launched during cleanup: %v", err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	saved, err := runtime.Workflow.Get(context.Background(), run.ID)
	if err != nil || saved.Merge.Pending() || !saved.Merge.WorktreeRemoved || !saved.Merge.BranchDeleted {
		t.Fatalf("cleanup did not finish: %+v %v", saved.Merge, err)
	}
}

func TestDashboardCopiedTakeoverProtectsLiveWorktree(t *testing.T) {

	binary := filepath.Join(t.TempDir(), "mergeyard's <launcher>")
	build := exec.Command("go", "build", "-o", binary, testkit.RepositoryPath("cmd/mergeyard"))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI fixture: %s %v", output, err)
	}
	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			flow, script := testkit.TmuxFlow, testkit.SuccessfulScript
			if agent == "codex" {
				flow, script = testkit.TmuxCodexFlow, testkit.CodexImplementation
			}
			_, runtime, base, _, cfg, r := flow(t, script)
			api := &testkit.MergeGitHub{CiGitHub: &testkit.CiGitHub{FakeGitHub: base}, ApplyClose: true}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
			listener, err := web.Listen("127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			relativeWorkspace, err := filepath.Rel(cwd, runtime.Workspace.Root)
			if err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(t.TempDir(), "config's <source>.yaml")
			relativeConfig, err := filepath.Rel(cwd, configPath)
			if err != nil {
				t.Fatal(err)
			}
			port := listener.Addr().(*net.TCPAddr).Port
			// The CLI rebuilds the resume invocation from its own configuration,
			// so the copied command's config must name the same harness executables
			// as the runtime's in-memory configuration.
			configText := fmt.Sprintf("workspace: %q\nport: %d\nagents:\n  claude:\n    executable: %q\n  codex:\n    executable: %q\n",
				relativeWorkspace, port, cfg.Agents.Claude.Executable, cfg.Agents.Codex.Executable)
			if err := os.WriteFile(configPath, []byte(configText), 0600); err != nil {
				t.Fatal(err)
			}
			server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{
				DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg,
				ConfigPath: relativeConfig, CLIExecutable: binary,
			})
			if err != nil {
				listener.Close()
				t.Fatal(err)
			}
			httpServer := httptest.NewUnstartedServer(server)
			httpServer.Listener = listener
			httpServer.Start()
			defer httpServer.Close()
			statusResponse := httptest.NewRecorder()
			server.ServeHTTP(statusResponse, testkit.ApiRequest(t, runtime.Workspace.Root, "GET", httpServer.URL+"/api/status"))
			var status web.Status
			if err := json.Unmarshal(statusResponse.Body.Bytes(), &status); err != nil {
				t.Fatal(err)
			}
			request := testkit.DashboardRequest(t, server, "POST", httpServer.URL+"/runs/"+run.ID+"/takeover", nil)
			request.Header.Set("Origin", httpServer.URL)
			request.Header.Set("X-CSRF-Token", status.Token)
			request.Header.Set("HX-Request", "true")
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != 200 {
				t.Fatalf("dashboard takeover: %d %s", response.Code, response.Body.String())
			}
			matches := regexp.MustCompile(`data-copy="([^"]*)">Copy resume command</button>`).FindStringSubmatch(response.Body.String())
			if len(matches) != 2 {
				t.Fatal("dashboard did not offer a copyable resume command")
			}
			copied := html.UnescapeString(matches[1])
			command, err := s.ManualCommand(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			gate := t.TempDir()
			started, release, arguments := filepath.Join(gate, "started"), filepath.Join(gate, "release"), filepath.Join(gate, "arguments")
			quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
			fake := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$PWD\" \"$@\" > %s\nprintf ready > %s\nwhile [ ! -f %s ]; do /bin/sleep 0.02; done\n", quote(arguments), quote(started), quote(release))
			if err := os.WriteFile(command.Executable, []byte(fake), 0700); err != nil {
				t.Fatal(err)
			}
			child := exec.Command("/bin/sh", "-c", copied)
			child.Dir = t.TempDir()
			child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			child.Stdout, child.Stderr = io.Discard, io.Discard
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				os.WriteFile(release, nil, 0600)
				if child.ProcessState == nil {
					syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
					child.Wait()
				}
			})
			testkit.WaitFor(t, func() bool { _, err := os.Stat(started); return err == nil })
			data, err := os.ReadFile(arguments)
			expectedArgs := []string{"--resume", run.Implementer.SessionID, "--permission-mode", "default"}
			if agent == "codex" {
				expectedArgs = []string{"resume", "-C", command.Dir, "-a", "on-request", "-s", "workspace-write", "--", run.Implementer.SessionID}
			}
			if err != nil || string(data) != command.Dir+"\n"+strings.Join(expectedArgs, "\n")+"\n" {
				t.Fatalf("copied command lost exact session, directory or permissions: %q %v", data, err)
			}
			api.Prs["mergeyard/issue-7"].State, api.Prs["mergeyard/issue-7"].Merged = github.Closed, true
			if _, err := s.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Kill(-child.Process.Pid, 0); err != nil {
				t.Fatalf("copied-command process exited before release: %v", err)
			}
			if _, err := os.Stat(command.Dir); err != nil {
				t.Fatalf("dashboard command left live worktree unprotected: %v", err)
			}
			saved, err := runtime.Workflow.Get(ctx, run.ID)
			if err != nil || !saved.Merge.Pending() || saved.Merge.WorktreeRemoved || saved.Merge.BranchDeleted {
				t.Fatalf("dashboard merge cleanup: %+v %v", saved.Merge, err)
			}
			httpServer.Close()
			root := runtime.Workspace.Root
			if err := runtime.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := app.Open(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			next, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(reopened), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := next.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(command.Dir); err != nil {
				t.Fatalf("restart removed dashboard session's worktree: %v", err)
			}
			if err := os.WriteFile(release, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := child.Wait(); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if _, err := next.Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
			}
			saved, err = reopened.Workflow.Get(ctx, run.ID)
			if err != nil || saved.Merge.Pending() {
				t.Fatalf("dashboard cleanup did not finish after exit: %+v %v", saved.Merge, err)
			}
			if _, err := os.Stat(command.Dir); !os.IsNotExist(err) {
				t.Fatalf("worktree retained after dashboard session exit: %v", err)
			}
		})
	}
}
