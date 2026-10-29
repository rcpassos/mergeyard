package integration_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestTakeoverStopsImplementerAndResumesExactConversation(t *testing.T) {

	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			flow, script := testkit.TmuxFlow, `printf keep > pending.txt; sleep 60`
			if agent == "codex" {
				flow, script = testkit.TmuxCodexFlow, testkit.CodexIdentity+"\n"+script
			}
			s, runtime, api, _, cfg, r := flow(t, script)
			ctx := context.Background()
			if err := s.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			runs, _ := s.Runs(ctx)
			run := runs[0]
			if agent == "codex" {
				run = testkit.WaitCodexIdentity(t, s)
			}
			ref, err := s.Watch(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			testkit.WaitFor(t, func() bool {
				_, err := os.Stat(filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", run.ID, "pending.txt"))
				return err == nil
			})
			prepared, err := s.Takeover(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			manual, err := runtime.Workflow.Get(ctx, run.ID)
			if err != nil || manual.State != workflow.Manual || manual.Implementer.SessionID != run.Implementer.SessionID || prepared.ID != run.ID || prepared.State != workflow.Manual {
				t.Fatalf("manual: %+v %v", manual, err)
			}
			status, err := r.SessionStatus(ctx, ref)
			if err != nil || status.State == runner.SessionRunning {
				t.Fatalf("process survived: %+v %v", status, err)
			}
			worktree := testkit.RunWorktree(t, runtime.Workspace.Root, run)
			command := testkit.ManualCommand(t, s, run.ID)
			want := []string{"--resume", run.Implementer.SessionID, "--permission-mode", "default"}
			if agent == "codex" {
				want = []string{"resume", "-C", worktree, "-a", "on-request", "-s", "workspace-write", "--", testkit.CodexID}
			}
			if command.Dir != worktree || !reflect.DeepEqual(command.Args, want) {
				t.Fatalf("interactive arguments: %+v", command)
			}
			data, err := os.ReadFile(filepath.Join(worktree, "pending.txt"))
			if err != nil || string(data) != "keep" {
				t.Fatalf("lost partial work: %q %v", data, err)
			}
			again, err := s.Takeover(ctx, run.ID)
			if err != nil || again.ID != prepared.ID || again.State != workflow.Manual {
				t.Fatalf("repeated takeover: %+v %v", again, err)
			}
			root := runtime.Workspace.Root
			if err := runtime.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, err := app.Open(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			next, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(restarted), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := next.Tick(ctx); err != nil {
					t.Fatal(err)
				}
			}
			runs, err = next.Runs(ctx)
			if err != nil || len(runs) != 1 || runs[0].State != workflow.Manual || runs[0].Implementer.Attempt != 1 || api.Creations != 0 {
				t.Fatalf("manual restart launched work: %+v %v", runs, err)
			}
			history, err := restarted.Events.History(ctx, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			requested, entered := 0, 0
			for _, event := range history {
				if event.Type == "run.takeover_requested" {
					requested++
				}
				if event.Type == "run.manual" {
					entered++
				}
			}
			if requested != 1 || entered != 1 {
				t.Fatalf("duplicate takeover effects: %d %d", requested, entered)
			}
		})
	}
}

func TestTakeoverIntentFinishesAfterRestart(t *testing.T) {

	s, runtime, api, _, cfg, r := testkit.TmuxFlow(t, `printf keep > pending.txt; sleep 60`)
	ctx := context.Background()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(ctx)
	run := runs[0]
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	interrupted, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: testkit.InterruptedTakeoverRunner{Runner: r, Cancel: cancel}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := interrupted.Takeover(requestCtx, run.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("interruption: %v", err)
	}
	run, err = runtime.Workflow.Get(ctx, run.ID)
	if err != nil || run.TakeoverStatus != workflow.TakeoverRequested || run.State != workflow.Active {
		t.Fatalf("intent missing: %+v %v", run, err)
	}
	root := runtime.Workspace.Root
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := app.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	next, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(restarted), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := next.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	runs, err = next.Runs(ctx)
	if err != nil || runs[0].State != workflow.Manual || runs[0].Implementer.Attempt != 1 {
		t.Fatalf("restart resumed automation: %+v %v", runs, err)
	}
}

func TestTakeoverPrerequisitesDoNotInterruptOrCreateConversation(t *testing.T) {

	ctx := context.Background()
	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}}
	s, runtime := testkit.Configured(t, api, "repositories:\n  - repo: owner/repo\n", testkit.RejectingGit{})
	if _, err := runtime.Workflow.Transition(ctx, "unprepared", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7}); err != nil {
		t.Fatal(err)
	}
	prepared, err := s.Takeover(ctx, "unprepared")
	if !strings.Contains(fmt.Sprint(err), "takeover.worktree_missing") || prepared.ID != "" {
		t.Fatalf("missing worktree: %+v %v", prepared, err)
	}
	run, _ := runtime.Workflow.Get(ctx, "unprepared")
	if run.State != workflow.Claiming || run.TakeoverStatus != "" {
		t.Fatalf("prerequisite changed run: %+v", run)
	}

	s, runtime, _, _, _, r := testkit.TmuxCodexFlow(t, `sleep 60`)
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(ctx)
	run = runs[0]
	ref, err := s.Watch(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err = s.Takeover(ctx, run.ID)
	if !strings.Contains(fmt.Sprint(err), "takeover.session_missing") || prepared.ID != "" {
		t.Fatalf("missing session: %+v %v", prepared, err)
	}
	status, err := r.SessionStatus(ctx, ref)
	if err != nil || status.State != runner.SessionRunning {
		t.Fatalf("prerequisite interrupted process: %+v %v", status, err)
	}
	run, _ = runtime.Workflow.Get(ctx, run.ID)
	if run.State != workflow.Active || run.TakeoverStatus != "" {
		t.Fatalf("missing identity changed run: %+v", run)
	}
	if err := s.Stop(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Takeover(ctx, run.ID); !strings.Contains(fmt.Sprint(err), "takeover.unavailable") {
		t.Fatalf("terminal takeover: %v", err)
	}
}

func TestTakeoverAmbiguousExitPreservesWorkForAttention(t *testing.T) {

	s, runtime, api, _, cfg, r := testkit.TmuxFlow(t, `printf keep > pending.txt; sleep 60`)
	ctx := context.Background()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(ctx)
	run := runs[0]
	path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", run.ID, "pending.txt")
	testkit.WaitFor(t, func() bool { _, err := os.Stat(path); return err == nil })
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: testkit.AmbiguousTakeoverRunner{Runner: r}})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := s.Takeover(ctx, run.ID)
	if err == nil || prepared.ID != "" {
		t.Fatalf("ambiguous exit exposed command: %+v %v", prepared, err)
	}
	run, err = runtime.Workflow.Get(ctx, run.ID)
	if err != nil || run.State != workflow.NeedsAttention || run.TakeoverStatus != workflow.TakeoverAttention || run.LastErrorCode != "takeover.process_ambiguous" {
		t.Fatalf("ambiguity not retained: %+v %v", run, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "keep" {
		t.Fatalf("lost work: %q %v", data, err)
	}
	if _, err := s.ManualCommand(ctx, run.ID); err == nil {
		t.Fatal("command offered before safe takeover")
	}
}

func TestDashboardTakeoverPreparesAndDisplaysEscapedExactCommand(t *testing.T) {

	s, runtime, api, _, cfg, r := testkit.TmuxFlow(t, `sleep 60`)
	ctx := context.Background()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(ctx)
	id := runs[0].ID
	configPath := filepath.Join(t.TempDir(), "config's <source>.yaml")
	server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg, ConfigPath: configPath, CLIExecutable: "/tmp/mergeyard"})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, testkit.ApiRequest(t, runtime.Workspace.Root, "GET", "http://127.0.0.1:7331/api/status"))
	var status web.Status
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	for _, attack := range []string{"missing token", "foreign origin"} {
		request := testkit.DashboardRequest(t, server, "POST", "http://127.0.0.1:7331/runs/"+id+"/takeover", nil)
		request.Header.Set("Origin", "http://127.0.0.1:7331")
		if attack == "foreign origin" {
			request.Header.Set("X-CSRF-Token", status.Token)
			request.Header.Set("Origin", "https://attacker.example")
		}
		response = httptest.NewRecorder()
		server.ServeHTTP(response, request)
		run, _ := runtime.Workflow.Get(ctx, id)
		if response.Code != http.StatusForbidden || run.State != workflow.Active || run.TakeoverStatus != "" {
			t.Fatalf("%s changed run: %d %+v", attack, response.Code, run)
		}
	}
	request := testkit.DashboardRequest(t, server, "POST", "http://127.0.0.1:7331/runs/"+id+"/takeover", nil)
	request.Header.Set("Origin", "http://127.0.0.1:7331")
	request.Header.Set("X-CSRF-Token", status.Token)
	request.Header.Set("HX-Request", "true")
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	run, _ := runtime.Workflow.Get(ctx, id)
	if response.Code != http.StatusOK || run.State != workflow.Manual || !strings.Contains(response.Body.String(), "Copy resume command") || !strings.Contains(response.Body.String(), run.Implementer.SessionID) || !strings.Contains(response.Body.String(), "--config") {
		t.Fatalf("dashboard takeover: %d %+v %s", response.Code, run, response.Body.String())
	}
	if strings.Contains(response.Body.String(), ">Take over run</button>") {
		t.Fatal("manual state still offers initial takeover")
	}

	// Repeated protected API requests return the prepared run snapshot without
	// new work and never a command to run.
	request = httptest.NewRequest("POST", "http://127.0.0.1:7331/api/runs/"+id+"/takeover", nil)
	request.Header.Set("Origin", "http://127.0.0.1:7331")
	request.Header.Set("X-CSRF-Token", status.Token)
	testkit.SignAPIRequest(t, runtime.Workspace.Root, request)
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	var prepared workflow.Run
	if response.Code != 200 {
		t.Fatalf("takeover API: %d %s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &prepared); err != nil {
		t.Fatal(err)
	}
	if prepared.ID != id || prepared.State != workflow.Manual || prepared.Implementer.SessionID != run.Implementer.SessionID {
		t.Fatalf("API lost the prepared run identity: %+v", prepared)
	}
	if strings.Contains(response.Body.String(), `"executable"`) || strings.Contains(response.Body.String(), `"args"`) {
		t.Fatalf("API response carried a command: %s", response.Body.String())
	}
	// Configured executable paths can contain quotes and HTML syntax.
	cfg.Agents.Claude.Executable = "/tmp/harness's <script>alert(1)</script>"
	s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	server, err = web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg, ConfigPath: configPath, CLIExecutable: "/tmp/mergeyard's <script>alert(1)</script>"})
	if err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	server.ServeHTTP(response, testkit.DashboardRequest(t, server, "GET", "http://127.0.0.1:7331/runs/"+id, nil))
	if response.Code != 200 || strings.Contains(response.Body.String(), "<script>alert(1)</script>") || !strings.Contains(response.Body.String(), "&lt;script&gt;") {
		t.Fatalf("unescaped command: %d %s", response.Code, response.Body.String())
	}

	// Missing launcher metadata must never fall back to direct harness execution.
	server, err = web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	server.ServeHTTP(response, testkit.DashboardRequest(t, server, "GET", "http://127.0.0.1:7331/runs/"+id, nil))
	if response.Code != 200 || strings.Contains(response.Body.String(), "Copy resume command") || !strings.Contains(response.Body.String(), "configuration path") {
		t.Fatalf("missing config exposed unprotected launcher: %d %s", response.Code, response.Body.String())
	}
}

func TestTakeoverRestoresReviewerBeforeOfferingImplementer(t *testing.T) {

	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			t.Run(implementer+"/"+reviewer, func(t *testing.T) {

				s, runtime, _, _, cfg, _ := testkit.TmuxPairingFlow(t, implementer, reviewer, testkit.DisputedReport)
				run := testkit.Finish(t, s, workflow.Active, workflow.Review)
				testkit.ReplacePhaseScript(t, cfg, workflow.Review, testkit.ReviewScript(`printf tamper > feature.txt; printf reviewer > review-only.txt; sleep 60`))
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", run.ID)
				testkit.WaitFor(t, func() bool { _, err := os.Stat(filepath.Join(path, "review-only.txt")); return err == nil })
				prepared, err := s.Takeover(context.Background(), run.ID)
				if err != nil {
					t.Fatal(err)
				}
				manual, err := runtime.Workflow.Get(context.Background(), run.ID)
				command := testkit.ManualCommand(t, s, run.ID)
				if err != nil || manual.State != workflow.Manual || !manual.Review.Restored || manual.Review.Accepted || manual.Review.SessionID == manual.Implementer.SessionID || prepared.ID != run.ID || prepared.State != workflow.Manual || !strings.Contains(strings.Join(command.Args, " "), manual.Implementer.SessionID) {
					t.Fatalf("review takeover: %+v %v %+v", manual, err, command)
				}
				data, err := os.ReadFile(filepath.Join(path, "feature.txt"))
				if err != nil || string(data) != "implemented\n" {
					t.Fatalf("review edits retained: %q %v", data, err)
				}
				if _, err := os.Stat(filepath.Join(path, "review-only.txt")); !os.IsNotExist(err) {
					t.Fatalf("review file not restored: %v", err)
				}
				history, err := runtime.Events.History(context.Background(), 0, 100)
				if err != nil {
					t.Fatal(err)
				}
				restored := false
				for _, event := range history {
					if event.Type == "review.restored" {
						restored = true
					}
					if event.Type == "run.manual" && !restored {
						t.Fatal("manual exposed before restoration")
					}
				}
			})
		}
	}
}

func TestTakeoverFixPreservesPartialImplementerChanges(t *testing.T) {

	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {

			s, runtime, _, _, _, _ := testkit.TmuxPairingFlow(t, agent, "claude", `printf manual > feature.txt; printf kept > fix-only.txt; sleep 60`)
			run := testkit.Finish(t, s, workflow.Active, workflow.Fix)
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", run.ID)
			testkit.WaitFor(t, func() bool { _, err := os.Stat(filepath.Join(path, "fix-only.txt")); return err == nil })
			prepared, err := s.Takeover(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			manual, err := runtime.Workflow.Get(context.Background(), run.ID)
			command := testkit.ManualCommand(t, s, run.ID)
			if err != nil || manual.State != workflow.Manual || manual.Fix.SessionID != manual.Implementer.SessionID || prepared.State != workflow.Manual || !strings.Contains(strings.Join(command.Args, " "), manual.Implementer.SessionID) {
				t.Fatalf("fix takeover: %+v %+v %v", manual, command, err)
			}
			data, err := os.ReadFile(filepath.Join(path, "feature.txt"))
			if err != nil || string(data) != "manual" {
				t.Fatalf("partial fix lost: %q %v", data, err)
			}
		})
	}
}

func TestTakeoverRecoversAfterControlPlaneKill(t *testing.T) {

	for _, stage := range []string{"stopped", "restored"} {
		t.Run(stage, func(t *testing.T) {

			s, initial, api, _, cfg, _ := testkit.TmuxFlow(t, testkit.ReviewScript(`printf reviewer > review-only.txt; printf tamper > feature.txt; sleep 60`))
			run := testkit.Finish(t, s, workflow.Active, workflow.Review)
			root := initial.Workspace.Root
			if err := initial.Close(); err != nil {
				t.Fatal(err)
			}
			socket := fmt.Sprintf("mergeyard-takeover-restart-%d", time.Now().UnixNano())
			t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
			// Launch the reviewer on a known socket before starting the control-plane
			// subprocess. Both owners recover the same persisted execution.
			setup, err := app.Open(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			phaseRunner := runner.NewLocal(runner.Options{SocketName: socket})
			setupScheduler, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(setup), scheduler.Dependencies{GitHub: api, Runner: phaseRunner})
			if err != nil {
				t.Fatal(err)
			}
			if err := setupScheduler.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "worktrees", "owner-repo", run.ID)
			testkit.WaitFor(t, func() bool { _, err := os.Stat(filepath.Join(path, "review-only.txt")); return err == nil })
			if err := setup.Close(); err != nil {
				t.Fatal(err)
			}
			input, _ := json.Marshal(testkit.ReviewProcessInput{Config: cfg, Workspace: root, Socket: socket, PR: api.Prs["mergeyard/issue-7"], TakeoverCrash: stage, KillDuringRestore: stage == "restored"})
			inputPath := filepath.Join(t.TempDir(), "input.json")
			if err := os.WriteFile(inputPath, input, 0600); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestReviewControlPlaneProcess$")
			cmd.Env = append(os.Environ(), "MERGEYARD_TEST_REVIEW_PROCESS="+inputPath)
			cmd.Stderr = os.Stderr
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
			scanner := bufio.NewScanner(stdout)
			if !scanner.Scan() || scanner.Text() != "review-control-plane-ready" {
				t.Fatalf("subprocess not ready: %q %v", scanner.Text(), scanner.Err())
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			cmd.Wait()
			restarted, err := app.Open(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			pending, err := restarted.Workflow.Get(context.Background(), run.ID)
			if err != nil || pending.TakeoverStatus != workflow.TakeoverRequested || pending.State == workflow.Manual {
				t.Fatalf("unsafe crash outcome: %+v %v", pending, err)
			}
			next, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(restarted), scheduler.Dependencies{GitHub: api, Runner: phaseRunner})
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if _, err := next.Reconcile(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			manual, err := restarted.Workflow.Get(context.Background(), run.ID)
			if err != nil || manual.State != workflow.Manual || !manual.Review.Restored || manual.Review.Attempt != 1 || manual.Implementer.SessionID != run.Implementer.SessionID || manual.ApprovedSHA != "" {
				t.Fatalf("lost restoration or duplicated work: %+v %v", manual, err)
			}
			if _, err := next.ManualCommand(context.Background(), run.ID); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(path, "feature.txt"))
			if err != nil || string(data) != "implemented\n" {
				t.Fatalf("lost restored work: %q %v", data, err)
			}
			if _, err := os.Stat(filepath.Join(path, "review-only.txt")); !os.IsNotExist(err) {
				t.Fatalf("review file survived: %v", err)
			}
		})
	}
}

func TestTakeoverWaitsForLifecycleOperationAndConcurrentRequestsAreIdempotent(t *testing.T) {

	s, runtime, _, _, _, _ := testkit.TmuxFlow(t, `sleep 60`)
	ctx := context.Background()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(ctx)
	id := runs[0].ID
	held, release, operationDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		operationDone <- runtime.Workflow.WithRunOperation(ctx, id, func(context.Context, workflow.Run) error { close(held); <-release; return nil })
	}()
	<-held
	type result struct {
		prepared workflow.Run
		ErrHook  error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() { prepared, err := s.Takeover(ctx, id); results <- result{prepared, err} }()
	}
	select {
	case got := <-results:
		t.Fatalf("takeover raced in-flight operation: %+v", got)
	case <-time.After(30 * time.Millisecond):
	}
	before, err := runtime.Workflow.Get(ctx, id)
	if err != nil || before.State != workflow.Active || before.TakeoverStatus != "" {
		t.Fatalf("takeover effects before operation release: %+v %v", before, err)
	}
	close(release)
	if err := <-operationDone; err != nil {
		t.Fatal(err)
	}
	first, second := <-results, <-results
	if first.ErrHook != nil || second.ErrHook != nil || first.prepared.ID != id || first.prepared.State != workflow.Manual || !reflect.DeepEqual(first.prepared.Implementer, second.prepared.Implementer) {
		t.Fatalf("concurrent takeover: %+v %+v", first, second)
	}
	history, err := runtime.Events.History(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	requested, entered := 0, 0
	for _, event := range history {
		if event.Type == "run.takeover_requested" {
			requested++
		}
		if event.Type == "run.manual" {
			entered++
		}
	}
	if requested != 1 || entered != 1 {
		t.Fatalf("duplicate effects: %d %d", requested, entered)
	}
}

func TestTakeoverPreservesAmbiguousReviewerGitState(t *testing.T) {

	s, runtime, _, _, _, _ := testkit.TmuxFlow(t, testkit.ReviewScript(`git checkout -b unexpected-review-branch; printf reviewer > review-only.txt; sleep 60`))
	run := testkit.Finish(t, s, workflow.Active, workflow.Review)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", run.ID)
	testkit.WaitFor(t, func() bool { _, err := os.Stat(filepath.Join(path, "review-only.txt")); return err == nil })
	prepared, err := s.Takeover(context.Background(), run.ID)
	if err == nil || prepared.ID != "" {
		t.Fatalf("ambiguous Git offered control: %+v %v", prepared, err)
	}
	attention, err := runtime.Workflow.Get(context.Background(), run.ID)
	if err != nil || attention.State != workflow.NeedsAttention || attention.TakeoverStatus != workflow.TakeoverAttention || attention.Review.Restored {
		t.Fatalf("unsafe restoration: %+v %v", attention, err)
	}
	data, err := os.ReadFile(filepath.Join(path, "review-only.txt"))
	if err != nil || string(data) != "reviewer" || testkit.GitCommand(t, path, "branch", "--show-current") != "unexpected-review-branch" {
		t.Fatalf("ambiguous work overwritten: %q %v", data, err)
	}
	if _, err := s.ManualCommand(context.Background(), run.ID); err == nil {
		t.Fatal("unsafe manual command")
	}
}

func TestManualResumeRejectsWorktreeReplacementAndPreservesBothRepositories(t *testing.T) {

	s, runtime, _, _, _, _ := testkit.TmuxFlow(t, `printf keep > pending.txt; sleep 60`)
	ctx := context.Background()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(ctx)
	id := runs[0].ID
	path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", id)
	testkit.WaitFor(t, func() bool { _, err := os.Stat(filepath.Join(path, "pending.txt")); return err == nil })
	if _, err := s.Takeover(ctx, id); err != nil {
		t.Fatal(err)
	}
	preserved := path + "-preserved"
	if err := os.Rename(path, preserved); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(runtime.Workspace.Root), "seed")
	before := testkit.GitCommand(t, outside, "rev-parse", "HEAD")
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ManualCommand(ctx, id); err == nil {
		t.Fatal("dashboard offered a command outside the owned worktree")
	}
	prepared, err := s.Takeover(ctx, id)
	if err == nil || prepared.ID != "" {
		t.Fatalf("CLI resumed in another repository: %+v %v", prepared, err)
	}
	attention, err := runtime.Workflow.Get(ctx, id)
	if err != nil || attention.State != workflow.NeedsAttention {
		t.Fatalf("replacement not diagnosed: %+v %v", attention, err)
	}
	data, err := os.ReadFile(filepath.Join(preserved, "pending.txt"))
	if err != nil || string(data) != "keep" || testkit.GitCommand(t, outside, "rev-parse", "HEAD") != before {
		t.Fatalf("repository replacement lost work: %q %v", data, err)
	}
}
