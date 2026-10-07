package scheduler_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/web"
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
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestTakeoverStopsImplementerAndResumesExactConversation(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			flow, script := localFlow, `printf keep > pending.txt; sleep 60`
			if agent == "codex" {
				flow, script = codexFlow, codexIdentity+"\n"+script
			}
			s, runtime, api, _, cfg, r := flow(t, script)
			ctx := context.Background()
			if err := s.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			runs, _ := s.Runs(ctx)
			run := runs[0]
			if agent == "codex" {
				run = waitCodexIdentity(t, s)
			}
			ref, err := s.Watch(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool {
				_, err := os.Stat(filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", run.ID, "pending.txt"))
				return err == nil
			})
			command, err := s.Takeover(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			manual, err := runtime.Workflow.Get(ctx, run.ID)
			if err != nil || manual.State != workflow.Manual || manual.Implementer.SessionID != run.Implementer.SessionID {
				t.Fatalf("manual: %+v %v", manual, err)
			}
			status, err := r.SessionStatus(ctx, ref)
			if err != nil || status.State == runner.SessionRunning {
				t.Fatalf("process survived: %+v %v", status, err)
			}
			want := []string{"--resume", run.Implementer.SessionID, "--permission-mode", "default"}
			if agent == "codex" {
				want = []string{"resume", "-C", command.Dir, "-a", "on-request", "-s", "workspace-write", "--", codexID}
			}
			if !reflect.DeepEqual(command.Args, want) {
				t.Fatalf("interactive arguments: %q", command.Args)
			}
			data, err := os.ReadFile(filepath.Join(command.Dir, "pending.txt"))
			if err != nil || string(data) != "keep" {
				t.Fatalf("lost partial work: %q %v", data, err)
			}
			again, err := s.Takeover(ctx, run.ID)
			if err != nil || !reflect.DeepEqual(again, command) {
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
			next, err := scheduler.New(cfg, schedulerResources(restarted), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := next.Tick(ctx); err != nil {
					t.Fatal(err)
				}
			}
			runs, err = next.Runs(ctx)
			if err != nil || len(runs) != 1 || runs[0].State != workflow.Manual || runs[0].Implementer.Attempt != 1 || api.creations != 0 {
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

// The runner models an interrupted HTTP request after intent has been saved.
type interruptedTakeoverRunner struct {
	runner.Runner
	cancel context.CancelFunc
}

func (r interruptedTakeoverRunner) StopSession(ctx context.Context, ref runner.SessionRef) error {
	r.cancel()
	return ctx.Err()
}

func TestTakeoverIntentFinishesAfterRestart(t *testing.T) {
	s, runtime, api, _, cfg, r := localFlow(t, `printf keep > pending.txt; sleep 60`)
	ctx := context.Background()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(ctx)
	run := runs[0]
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	interrupted, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: interruptedTakeoverRunner{r, cancel}})
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
	next, err := scheduler.New(cfg, schedulerResources(restarted), scheduler.Dependencies{GitHub: api, Runner: r})
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
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}}
	s, runtime := configured(t, api, "repositories:\n  - repo: owner/repo\n", rejectingGit{})
	if _, err := runtime.Workflow.Transition(ctx, "unprepared", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7}); err != nil {
		t.Fatal(err)
	}
	command, err := s.Takeover(ctx, "unprepared")
	if !strings.Contains(fmt.Sprint(err), "takeover.worktree_missing") || command.Executable != "" {
		t.Fatalf("missing worktree: %+v %v", command, err)
	}
	run, _ := runtime.Workflow.Get(ctx, "unprepared")
	if run.State != workflow.Claiming || run.TakeoverStatus != "" {
		t.Fatalf("prerequisite changed run: %+v", run)
	}

	s, runtime, _, _, _, r := codexFlow(t, `sleep 60`)
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(ctx)
	run = runs[0]
	ref, err := s.Watch(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	command, err = s.Takeover(ctx, run.ID)
	if !strings.Contains(fmt.Sprint(err), "takeover.session_missing") || command.Executable != "" {
		t.Fatalf("missing session: %+v %v", command, err)
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

type ambiguousTakeoverRunner struct{ runner.Runner }

func (r ambiguousTakeoverRunner) SessionStatus(context.Context, runner.SessionRef) (runner.SessionStatus, error) {
	return runner.SessionStatus{State: runner.SessionMissing}, nil
}

func TestTakeoverAmbiguousExitPreservesWorkForAttention(t *testing.T) {
	s, runtime, api, _, cfg, r := localFlow(t, `printf keep > pending.txt; sleep 60`)
	ctx := context.Background()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(ctx)
	run := runs[0]
	path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", run.ID, "pending.txt")
	waitFor(t, func() bool { _, err := os.Stat(path); return err == nil })
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: ambiguousTakeoverRunner{r}})
	if err != nil {
		t.Fatal(err)
	}
	command, err := s.Takeover(ctx, run.ID)
	if err == nil || command.Executable != "" {
		t.Fatalf("ambiguous exit exposed command: %+v %v", command, err)
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
	s, runtime, api, _, cfg, r := localFlow(t, `sleep 60`)
	ctx := context.Background()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(ctx)
	id := runs[0].ID
	server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest("GET", "http://127.0.0.1:7331/api/status", nil))
	var status web.Status
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	for _, attack := range []string{"missing token", "foreign origin"} {
		request := httptest.NewRequest("POST", "http://127.0.0.1:7331/runs/"+id+"/takeover", nil)
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
	request := httptest.NewRequest("POST", "http://127.0.0.1:7331/runs/"+id+"/takeover", nil)
	request.Header.Set("Origin", "http://127.0.0.1:7331")
	request.Header.Set("X-CSRF-Token", status.Token)
	request.Header.Set("HX-Request", "true")
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	run, _ := runtime.Workflow.Get(ctx, id)
	if response.Code != http.StatusOK || run.State != workflow.Manual || !strings.Contains(response.Body.String(), "Copy resume command") || !strings.Contains(response.Body.String(), run.Implementer.SessionID) || !strings.Contains(response.Body.String(), "--permission-mode") {
		t.Fatalf("dashboard takeover: %d %+v %s", response.Code, run, response.Body.String())
	}
	if strings.Contains(response.Body.String(), ">Take over run</button>") {
		t.Fatal("manual state still offers initial takeover")
	}

	// Repeated protected API requests return the exact invocation without new work.
	request = httptest.NewRequest("POST", "http://127.0.0.1:7331/api/runs/"+id+"/takeover", nil)
	request.Header.Set("Origin", "http://127.0.0.1:7331")
	request.Header.Set("X-CSRF-Token", status.Token)
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	var command harness.InteractiveCommand
	if response.Code != 200 {
		t.Fatalf("takeover API: %d %s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &command); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(command.Args, " "), run.Implementer.SessionID) {
		t.Fatalf("API lost exact identity: %+v", command)
	}
	// Exercise the displayed command with a harmless fake interactive executable,
	// including shell metacharacters in its path.
	command.Executable = filepath.Join(t.TempDir(), "harness's $(printf injected) <script>")
	if err := os.WriteFile(command.Executable, []byte("#!/bin/sh\nprintf '%s\\n' \"$PWD\" \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("/bin/sh", "-c", command.ShellCommand()).CombinedOutput()
	if err != nil || string(output) != command.Dir+"\n"+strings.Join(command.Args, "\n")+"\n" {
		t.Fatalf("copy command quoting: %q %v", output, err)
	}
	// Configured executable paths can contain quotes and HTML syntax.
	cfg.Agents.Claude.Executable = "/tmp/harness's <script>alert(1)</script>"
	s, err = scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	server, err = web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest("GET", "http://127.0.0.1:7331/runs/"+id, nil))
	if response.Code != 200 || strings.Contains(response.Body.String(), "<script>alert(1)</script>") || !strings.Contains(response.Body.String(), "&lt;script&gt;") {
		t.Fatalf("unescaped command: %d %s", response.Code, response.Body.String())
	}
}

func TestTakeoverRestoresReviewerBeforeOfferingImplementer(t *testing.T) {
	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			t.Run(implementer+"/"+reviewer, func(t *testing.T) {
				s, runtime, _, _, cfg, _ := pairingFlow(t, implementer, reviewer, disputedReport)
				run := finish(t, s, workflow.Active, workflow.Review)
				replacePhaseScript(t, cfg, workflow.Review, reviewScript(`printf tamper > feature.txt; printf reviewer > review-only.txt; sleep 60`))
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", run.ID)
				waitFor(t, func() bool { _, err := os.Stat(filepath.Join(path, "review-only.txt")); return err == nil })
				command, err := s.Takeover(context.Background(), run.ID)
				if err != nil {
					t.Fatal(err)
				}
				manual, err := runtime.Workflow.Get(context.Background(), run.ID)
				if err != nil || manual.State != workflow.Manual || !manual.Review.Restored || manual.Review.Accepted || manual.Review.SessionID == manual.Implementer.SessionID || !strings.Contains(strings.Join(command.Args, " "), manual.Implementer.SessionID) {
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
			s, runtime, _, _, _, _ := pairingFlow(t, agent, "claude", `printf manual > feature.txt; printf kept > fix-only.txt; sleep 60`)
			run := finish(t, s, workflow.Active, workflow.Fix)
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", run.ID)
			waitFor(t, func() bool { _, err := os.Stat(filepath.Join(path, "fix-only.txt")); return err == nil })
			command, err := s.Takeover(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			manual, err := runtime.Workflow.Get(context.Background(), run.ID)
			if err != nil || manual.State != workflow.Manual || manual.Fix.SessionID != manual.Implementer.SessionID || !strings.Contains(strings.Join(command.Args, " "), manual.Implementer.SessionID) {
				t.Fatalf("fix takeover: %+v %+v %v", manual, command, err)
			}
			data, err := os.ReadFile(filepath.Join(path, "feature.txt"))
			if err != nil || string(data) != "manual" {
				t.Fatalf("partial fix lost: %q %v", data, err)
			}
		})
	}
}

// This crash occurs after the real runner has proved exit and before the
// scheduler can journal the manual outcome.
type takeoverCrashRunner struct{ runner.Runner }

func (r takeoverCrashRunner) StopSession(ctx context.Context, ref runner.SessionRef) error {
	if err := r.Runner.StopSession(ctx, ref); err != nil {
		return err
	}
	fmt.Println("review-control-plane-ready")
	select {}
}

func TestTakeoverRecoversAfterControlPlaneKill(t *testing.T) {
	for _, stage := range []string{"stopped", "restored"} {
		t.Run(stage, func(t *testing.T) {
			s, initial, api, _, cfg, _ := localFlow(t, reviewScript(`printf reviewer > review-only.txt; printf tamper > feature.txt; sleep 60`))
			run := finish(t, s, workflow.Active, workflow.Review)
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
			setupScheduler, err := scheduler.New(cfg, schedulerResources(setup), scheduler.Dependencies{GitHub: api, Runner: phaseRunner})
			if err != nil {
				t.Fatal(err)
			}
			if err := setupScheduler.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "worktrees", "owner-repo", run.ID)
			waitFor(t, func() bool { _, err := os.Stat(filepath.Join(path, "review-only.txt")); return err == nil })
			if err := setup.Close(); err != nil {
				t.Fatal(err)
			}
			input, _ := json.Marshal(reviewProcessInput{Config: cfg, Workspace: root, Socket: socket, PR: api.prs["mergeyard/issue-7"], TakeoverCrash: stage, KillDuringRestore: stage == "restored"})
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
			next, err := scheduler.New(cfg, schedulerResources(restarted), scheduler.Dependencies{GitHub: api, Runner: phaseRunner})
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
	s, runtime, _, _, _, _ := localFlow(t, `sleep 60`)
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
		command harness.InteractiveCommand
		err     error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() { command, err := s.Takeover(ctx, id); results <- result{command, err} }()
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
	if first.err != nil || second.err != nil || !reflect.DeepEqual(first.command, second.command) {
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
	s, runtime, _, _, _, _ := localFlow(t, reviewScript(`git checkout -b unexpected-review-branch; printf reviewer > review-only.txt; sleep 60`))
	run := finish(t, s, workflow.Active, workflow.Review)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", run.ID)
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(path, "review-only.txt")); return err == nil })
	command, err := s.Takeover(context.Background(), run.ID)
	if err == nil || command.Executable != "" {
		t.Fatalf("ambiguous Git offered control: %+v %v", command, err)
	}
	attention, err := runtime.Workflow.Get(context.Background(), run.ID)
	if err != nil || attention.State != workflow.NeedsAttention || attention.TakeoverStatus != workflow.TakeoverAttention || attention.Review.Restored {
		t.Fatalf("unsafe restoration: %+v %v", attention, err)
	}
	data, err := os.ReadFile(filepath.Join(path, "review-only.txt"))
	if err != nil || string(data) != "reviewer" || gitCommand(t, path, "branch", "--show-current") != "unexpected-review-branch" {
		t.Fatalf("ambiguous work overwritten: %q %v", data, err)
	}
	if _, err := s.ManualCommand(context.Background(), run.ID); err == nil {
		t.Fatal("unsafe manual command")
	}
}

func TestManualResumeRejectsWorktreeReplacementAndPreservesBothRepositories(t *testing.T) {
	s, runtime, _, _, _, _ := localFlow(t, `printf keep > pending.txt; sleep 60`)
	ctx := context.Background()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(ctx)
	id := runs[0].ID
	path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", id)
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(path, "pending.txt")); return err == nil })
	if _, err := s.Takeover(ctx, id); err != nil {
		t.Fatal(err)
	}
	preserved := path + "-preserved"
	if err := os.Rename(path, preserved); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(runtime.Workspace.Root), "seed")
	before := gitCommand(t, outside, "rev-parse", "HEAD")
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ManualCommand(ctx, id); err == nil {
		t.Fatal("dashboard offered a command outside the owned worktree")
	}
	command, err := s.Takeover(ctx, id)
	if err == nil || command.Executable != "" {
		t.Fatalf("CLI resumed in another repository: %+v %v", command, err)
	}
	attention, err := runtime.Workflow.Get(ctx, id)
	if err != nil || attention.State != workflow.NeedsAttention {
		t.Fatalf("replacement not diagnosed: %+v %v", attention, err)
	}
	data, err := os.ReadFile(filepath.Join(preserved, "pending.txt"))
	if err != nil || string(data) != "keep" || gitCommand(t, outside, "rev-parse", "HEAD") != before {
		t.Fatalf("repository replacement lost work: %q %v", data, err)
	}
}
