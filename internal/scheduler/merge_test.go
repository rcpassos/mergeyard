package scheduler_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/sessions"
	"github.com/rcpassos/mergeyard/internal/web"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestObservedMergeCompletesCoding(t *testing.T) {
	s, runtime, api, _, _, _ := localFlow(t, successfulScript)
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
	pr := api.prs["mergeyard/issue-7"]
	pr.State, pr.Merged = github.Closed, true
	api.issues["owner/repo"][0].State = github.Closed
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, err := runtime.Workflow.Get(context.Background(), run.ID)
	if err != nil || saved.State != workflow.Completed {
		t.Fatalf("merged coding run = %+v, %v", saved, err)
	}
}

func TestMergeCleanupPendingIsVisible(t *testing.T) {
	s, runtime, api, _, cfg, _ := localFlow(t, successfulScript)
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
	api.prs["mergeyard/issue-7"].State, api.prs["mergeyard/issue-7"].Merged = github.Closed, true
	api.mutate = func(string, string, int, string) error { return fmt.Errorf("GitHub unavailable <script>") }
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
		server.ServeHTTP(res, httptest.NewRequest("GET", "http://127.0.0.1:7331"+path, nil))
		if res.Code != 200 || !strings.Contains(res.Body.String(), "Merged — cleanup pending") {
			t.Fatalf("merge missing from %s", path)
		}
		if path != "/" && (!strings.Contains(res.Body.String(), "Remove running label") || strings.Contains(res.Body.String(), "unavailable <script>")) {
			t.Fatal("missing or unsafe maintenance details")
		}
	}
	res := httptest.NewRecorder()
	server.ServeHTTP(res, httptest.NewRequest("GET", "http://127.0.0.1:7331/api/status", nil))
	var status web.Status
	if err := json.Unmarshal(res.Body.Bytes(), &status); err != nil || len(status.Runs) != 1 || status.Runs[0].Merge == nil || !status.Runs[0].Merge.Pending() {
		t.Fatal("status lost maintenance")
	}
}

type mergeGitHub struct {
	*ciGitHub
	closeCalls int
	closeError error
	applyClose bool
}

func (f *mergeGitHub) CloseIssue(_ context.Context, repo string, n int) error {
	f.closeCalls++
	if f.applyClose {
		for i := range f.issues[repo] {
			if f.issues[repo][i].Number == n {
				f.issues[repo][i].State = github.Closed
			}
		}
	}
	return f.closeError
}

type pendingStopRunner struct {
	runner.Runner
	pending bool
	stopped int
}

func (r *pendingStopRunner) StopSession(ctx context.Context, ref runner.SessionRef) error {
	r.stopped++
	if r.pending {
		return nil
	}
	return r.Runner.StopSession(ctx, ref)
}

func TestEarlyMergeWaitsForProcessExitBeforeReleasingCapacity(t *testing.T) {
	_, runtime, base, remote, cfg, realRunner := localFlow(t, reviewScript(`printf tampered > feature.txt; /bin/sleep 60`))
	api := &mergeGitHub{ciGitHub: &ciGitHub{fakeGitHub: base}, applyClose: true}
	r := &pendingStopRunner{Runner: realRunner, pending: true}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.Active, workflow.Review)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	ref, err := s.Watch(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	head := gitCommand(t, remote, "rev-parse", "mergeyard/issue-7")
	base.prs["mergeyard/issue-7"].State, base.prs["mergeyard/issue-7"].Merged = github.Closed, true
	base.issues["owner/repo"] = append(base.issues["owner/repo"], ready(8))
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
	server.ServeHTTP(res, httptest.NewRequest("GET", "http://127.0.0.1:7331/runs/"+run.ID, nil))
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
	r.pending = false
	s, err = scheduler.New(cfg, schedulerResources(reopened), scheduler.Dependencies{GitHub: api, Runner: r})
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
	if gitCommand(t, remote, "rev-parse", "mergeyard/issue-7") != head {
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
	_, runtime, base, remote, cfg, r := localFlow(t, successfulScript)
	api := &mergeGitHub{ciGitHub: &ciGitHub{fakeGitHub: base}, applyClose: true}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
	var path string
	if err := runtime.DB.QueryRow("SELECT worktree_path FROM runs WHERE id=?", run.ID).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "human.txt"), []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	base.prs["mergeyard/issue-7"].State, base.prs["mergeyard/issue-7"].Merged = github.Closed, true
	base.issues["owner/repo"] = append(base.issues["owner/repo"], ready(8))
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
	s, err = scheduler.New(cfg, schedulerResources(reopened), scheduler.Dependencies{GitHub: api, Runner: r})
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
	if gitCommand(t, remote, "rev-parse", "mergeyard/issue-7") == "" {
		t.Fatal("remote branch removed")
	}
}

func TestMergeMaintenanceRestartsAtEachBoundary(t *testing.T) {
	for _, boundary := range []string{"merge_observed", "process_exited", "coding_completed", "ready_removed", "running_removed", "attention_removed", "issue_closed", "review_restored", "worktree_started", "worktree_removed", "branch_deleted"} {
		t.Run(boundary, func(t *testing.T) {
			_, runtime, base, _, cfg, r := localFlow(t, successfulScript)
			api := &mergeGitHub{ciGitHub: &ciGitHub{fakeGitHub: base}, applyClose: true}
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			run := finish(t, s, workflow.WaitingForCI, workflow.Review)
			pr := base.prs["mergeyard/issue-7"]
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
			s, err = scheduler.New(cfg, schedulerResources(reopened), scheduler.Dependencies{GitHub: api, Runner: r})
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
			if got := gitCommand(t, basePath, "branch", "--list", branch); got != "" {
				t.Fatal("local branch retained")
			}
			var attempts int
			if err := reopened.DB.QueryRow("SELECT count(*) FROM phase_attempts WHERE run_id=?", run.ID).Scan(&attempts); err != nil || attempts != 2 {
				t.Fatal("restart relaunched work")
			}
		})
	}
}

func TestAmbiguousMergeCleanupDoesNotRedispatchSourceIssue(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(fmt.Sprint(applied), func(t *testing.T) {
			_, runtime, base, _, cfg, r := localFlow(t, successfulScript)
			api := &mergeGitHub{ciGitHub: &ciGitHub{fakeGitHub: base}, applyClose: applied, closeError: fmt.Errorf("lost close response")}
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			run := finish(t, s, workflow.WaitingForCI, workflow.Review)
			base.prs["mergeyard/issue-7"].State, base.prs["mergeyard/issue-7"].Merged = github.Closed, true
			// The ready removal fails while other label removals succeed: pending
			// maintenance must still exclude this source issue from dispatch.
			base.issues["owner/repo"][0].Labels = append(base.issues["owner/repo"][0].Labels, github.Label{Name: "ready-for-agent"})
			base.mutate = func(action, repo string, n int, label string) error {
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
			base.issues["owner/repo"] = append(base.issues["owner/repo"], ready(8))
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
			calls := api.closeCalls
			if applied && calls != 1 {
				t.Fatal("applied closure was replayed")
			}
			base.mutate = nil
			api.applyClose = true
			api.closeError = nil
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
	for _, state := range []workflow.State{workflow.NeedsAttention, workflow.Manual, workflow.Stopped} {
		t.Run(string(state), func(t *testing.T) {
			s, runtime, api, _, _, _ := localFlow(t, successfulScript)
			run := finish(t, s, workflow.WaitingForCI, workflow.Review)
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
			api.prs["mergeyard/issue-7"].State, api.prs["mergeyard/issue-7"].Merged = github.Closed, true
			api.issues["owner/repo"][0].State = github.Closed
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

func TestUnexplainedMissingMergeWorktreeRequiresAttention(t *testing.T) {
	s, runtime, api, _, _, _ := localFlow(t, successfulScript)
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
	var path string
	runtime.DB.QueryRow("SELECT worktree_path FROM runs WHERE id=?", run.ID).Scan(&path)
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	api.prs["mergeyard/issue-7"].State, api.prs["mergeyard/issue-7"].Merged = github.Closed, true
	api.issues["owner/repo"][0].State = github.Closed
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

func TestReadyMergeAndCompletedPublicationRemainIndependent(t *testing.T) {
	_, runtime, base, _, cfg, r := localFlow(t, successfulScript)
	publisher := &reportGitHub{branchGitHub: branchGitHub{base}, failure: true}
	api := &mergedPublisher{mergeGitHub: &mergeGitHub{ciGitHub: &ciGitHub{fakeGitHub: base, applyReady: true, evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "success"}}}}, applyClose: true}, publisher: publisher}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.ReadyToMerge, workflow.Review)
	base.prs["mergeyard/issue-7"].State, base.prs["mergeyard/issue-7"].Merged = github.Closed, true
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, _ := runtime.Workflow.Get(context.Background(), run.ID)
	if saved.State != workflow.Completed || saved.Merge.Early || saved.Merge.Pending() || len(saved.Publications) != 1 || saved.Publications[0].State != "pending" {
		t.Fatalf("ready merge: %+v %+v", saved, saved.Merge)
	}
	publisher.failure = false
	for range 2 {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	saved, _ = runtime.Workflow.Get(context.Background(), run.ID)
	if len(publisher.bodies) != 1 || saved.Publications[0].State != "published" || saved.State != workflow.Completed {
		t.Fatal("cleanup blocked completed publication")
	}
}

type mergedPublisher struct {
	*mergeGitHub
	publisher *reportGitHub
}

func (f *mergedPublisher) EnsureReportComment(ctx context.Context, repo string, n int, body string) (*github.Comment, error) {
	return f.publisher.EnsureReportComment(ctx, repo, n, body)
}

func TestMergeStopsChildrenAfterWrapperExitedWhileOffline(t *testing.T) {
	// The reviewer starts a child that outlives its command and ignores soft
	// signals. exit.json and disappearance of tmux alone cannot prove exit.
	script := reviewScript(`case "$*" in *review-*) (trap '' INT TERM HUP; while :; do /bin/sleep 0.02; done) & printf '%s' "$!" > child.pid;; esac
` + approvedReview)
	s, runtime, api, _, cfg, r := localFlow(t, script)
	run := finish(t, s, workflow.Active, workflow.Review)
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
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(ref.PhaseDir, "exit.json")); return err == nil })
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
	api.prs["mergeyard/issue-7"].State, api.prs["mergeyard/issue-7"].Merged = github.Closed, true
	api.issues["owner/repo"][0].State = github.Closed
	s, err = scheduler.New(cfg, schedulerResources(reopened), scheduler.Dependencies{GitHub: api, Runner: r})
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

type unstartedReviewRunner struct{ runner.Runner }

func (r unstartedReviewRunner) StartSession(ctx context.Context, req runner.SessionRequest) (runner.SessionRef, error) {
	if req.Phase == "review" {
		return runner.SessionRef{Name: sessions.Name(req), PhaseDir: req.PhaseDir}, fmt.Errorf("interrupted before wrapper launch")
	}
	return r.Runner.StartSession(ctx, req)
}
func TestMergeRecoversRunningAttemptThatNeverLaunched(t *testing.T) {
	_, runtime, api, _, cfg, realRunner := localFlow(t, successfulScript)
	r := unstartedReviewRunner{realRunner}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.Active, workflow.Review)
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
	api.prs["mergeyard/issue-7"].State, api.prs["mergeyard/issue-7"].Merged = github.Closed, true
	api.issues["owner/repo"][0].State = github.Closed
	s, err = scheduler.New(cfg, schedulerResources(reopened), scheduler.Dependencies{GitHub: api, Runner: realRunner})
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
