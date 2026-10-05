package scheduler_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/config"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestOrphanedClaimIsReportedWithoutDispatchOrLabelChanges(t *testing.T) {
	issue := ready(7)
	issue.Labels = append(issue.Labels, github.Label{Name: "agent-running"})
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {issue}}}
	s, runtime := fixture(t, api)
	api.mutate = func(string, string, int, string) error { t.Fatal("orphan labels were changed"); return nil }
	for range 2 {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := s.Runs(context.Background())
	if err != nil || len(runs) != 0 {
		t.Fatalf("orphan dispatched: %v, %v", runs, err)
	}
	history, err := runtime.Events.History(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Type != "reconcile.orphaned_claim" {
		t.Fatalf("orphan report = %+v", history)
	}
	var finding scheduler.Finding
	if err := json.Unmarshal(history[0].Payload, &finding); err != nil {
		t.Fatal(err)
	}
	if finding.Repository != "owner/repo" || finding.IssueNumber != 7 {
		t.Fatalf("wrong orphan identity: %+v", finding)
	}
}

func TestOrphanedWorktreeAndSessionAreReportedAndPreservedWhilePaused(t *testing.T) {
	s, runtime, _, _, _, r := localFlow(t, successfulScript)
	ctx := context.Background()
	gitRun, err := managedgit.New(runtime.Workspace).Prepare(ctx, managedgit.PrepareRequest{Repository: "owner/repo", RunID: "orphan", IssueNumber: 42})
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(gitRun.Path, "keep.txt")
	if err := os.WriteFile(marker, []byte("preserve me"), 0600); err != nil {
		t.Fatal(err)
	}
	ref, err := r.StartSession(ctx, runner.SessionRequest{RunID: "orphan", Phase: "implement", Attempt: 1, PhaseDir: filepath.Join(runtime.Workspace.Root, "runs", "orphan", "phases", "implement-0-1"), Command: runner.ExecRequest{Executable: "/bin/sleep", Args: []string{"30"}, Dir: gitRun.Path}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Pause(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	history, err := runtime.Events.History(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	foundTree, foundSession := false, false
	for _, event := range history {
		var finding scheduler.Finding
		if err := json.Unmarshal(event.Payload, &finding); err != nil {
			t.Fatal(err)
		}
		if event.Type == "reconcile.orphaned_worktree" {
			actual, err := os.Stat(finding.Path)
			if err != nil {
				t.Fatal(err)
			}
			expected, err := os.Stat(gitRun.Path)
			if err != nil || !os.SameFile(actual, expected) {
				t.Fatalf("wrong orphan worktree: %+v, %v", finding, err)
			}
			foundTree = true
		}
		if event.Type == "reconcile.orphaned_session" && finding.Session == ref.Name {
			foundSession = true
		}
	}
	if !foundTree || !foundSession {
		t.Fatalf("missing orphan report: %+v", history)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "preserve me" {
		t.Fatalf("orphan files changed: %q, %v", data, err)
	}
	status, err := r.SessionStatus(ctx, ref)
	if err != nil || status.State != runner.SessionRunning {
		t.Fatalf("orphan session stopped: %+v, %v", status, err)
	}
	runs, err := s.Runs(ctx)
	if err != nil || len(runs) != 0 {
		t.Fatalf("reconciliation dispatched new work: %+v, %v", runs, err)
	}
}

func TestRunWithMissingContextKeepsItsOrphanedClaim(t *testing.T) {
	issue := ready(7)
	issue.Labels = []github.Label{{Name: "agent-running"}, {Name: "ready-for-agent"}}
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {issue}}}
	s, runtime := fixture(t, api)
	ctx := context.Background()
	for _, request := range []workflow.Request{{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7}, {Trigger: workflow.ClaimSucceeded}, {Trigger: workflow.WorktreeReady}} {
		if _, err := runtime.Workflow.Transition(ctx, "lost-context", request); err != nil {
			t.Fatal(err)
		}
	}
	api.mutate = func(string, string, int, string) error { t.Fatal("unrecoverable claim labels changed"); return nil }
	for range 2 {
		if err := s.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := s.Runs(ctx)
	if err != nil || len(runs) != 1 || runs[0].State != workflow.NeedsAttention || runs[0].LastErrorCode != "reconcile.context_missing" {
		t.Fatalf("unrecoverable run was resumed: %+v, %v", runs, err)
	}
	history, err := runtime.Events.History(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range history {
		if event.Type == "reconcile.orphaned_claim" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unrecoverable claim not reported: %+v", history)
	}
}

func TestMissingOwnedWorktreeRequiresAttentionWithoutReportingAnOrphan(t *testing.T) {
	s, runtime, _, _, _, _ := localFlow(t, `while [ ! -f release ]; do /bin/sleep 0.02; done`)
	ctx := context.Background()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	paths, _ := filepath.Glob(filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", "*"))
	if len(paths) != 1 {
		t.Fatalf("initial worktrees: %v", paths)
	}
	if err := os.RemoveAll(paths[0]); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(ctx)
	if err != nil || len(runs) != 1 || runs[0].State != workflow.NeedsAttention {
		t.Fatalf("missing worktree was resumed: %+v, %v", runs, err)
	}
	history, err := runtime.Events.History(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range history {
		if event.Type == "reconcile.orphaned_worktree" {
			t.Fatalf("missing owned worktree was reported as orphaned: %s", event.Payload)
		}
	}
}

type controlPlaneInput struct {
	Config    config.Config
	Workspace string
	Socket    string
}

// The parent kills this real control-plane process after its first dispatch.
// Agent and wrapper processes live on a separate tmux server and survive it.
func TestReconciliationControlPlaneProcess(t *testing.T) {
	path := os.Getenv("MERGEYARD_TEST_CONTROL_PLANE_INPUT")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var input controlPlaneInput
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	runtime, err := app.Open(context.Background(), input.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}}
	r := runner.NewLocal(runner.Options{SocketName: input.Socket})
	s, err := scheduler.New(input.Config, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(context.Background())
	if err != nil || len(runs) != 1 || runs[0].State != workflow.Active || runs[0].Phase != workflow.Implement {
		t.Fatalf("initial dispatch: %+v, %v", runs, err)
	}
	fmt.Println("control-plane-ready")
	time.Sleep(time.Minute)
}

func TestRestartAfterControlPlaneKillPreservesImplementAttempt(t *testing.T) {
	for _, finishedOffline := range []bool{false, true} {
		name := "agent-still-running"
		if finishedOffline {
			name = "agent-finished-offline"
		}
		t.Run(name, func(t *testing.T) {
			_, initial, api, remote, cfg, _ := localFlow(t, `while [ ! -f release ]; do /bin/sleep 0.02; done
`+successfulScript)
			root := initial.Workspace.Root
			if err := initial.Close(); err != nil {
				t.Fatal(err)
			}
			socket := fmt.Sprintf("mergeyard-reconciliation-test-%d", time.Now().UnixNano())
			t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
			input, err := json.Marshal(controlPlaneInput{Config: cfg, Workspace: root, Socket: socket})
			if err != nil {
				t.Fatal(err)
			}
			inputPath := filepath.Join(t.TempDir(), "control-plane.json")
			if err := os.WriteFile(inputPath, input, 0600); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestReconciliationControlPlaneProcess$")
			cmd.Env = append(os.Environ(), "MERGEYARD_TEST_CONTROL_PLANE_INPUT="+inputPath)
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
			if !scanner.Scan() || scanner.Text() != "control-plane-ready" {
				t.Fatalf("control plane never became ready: %q, %v", scanner.Text(), scanner.Err())
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			cmd.Wait()
			// GitHub claim labels outlive the killed process just as they do in production.
			api.issues["owner/repo"][0].Labels = []github.Label{{Name: "agent-running"}}
			trees, _ := filepath.Glob(filepath.Join(root, "worktrees", "owner-repo", "*"))
			if len(trees) != 1 {
				t.Fatalf("initial worktree count: %v", trees)
			}
			r := runner.NewLocal(runner.Options{SocketName: socket})
			var runtime *app.Runtime
			if finishedOffline {
				if err := os.WriteFile(filepath.Join(trees[0], "release"), nil, 0600); err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(5 * time.Second)
				for {
					exits, _ := filepath.Glob(filepath.Join(root, "runs", "*", "phases", "*", "exit.json"))
					if len(exits) == 1 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("agent did not exit while offline")
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			runtime, err = app.Open(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { runtime.Close() })
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			if !finishedOffline {
				for range 2 {
					if err := s.Tick(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				runs, err := s.Runs(context.Background())
				if err != nil || len(runs) != 1 || runs[0].State != workflow.Active || runs[0].Phase != workflow.Implement {
					t.Fatalf("live agent was not preserved: %+v, %v", runs, err)
				}
				if err := os.WriteFile(filepath.Join(trees[0], "release"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			run := finish(t, s, workflow.Active, workflow.Review)
			for range 2 {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			runs, err := s.Runs(context.Background())
			phases, _ := filepath.Glob(filepath.Join(root, "runs", run.ID, "phases", "*"))
			if err != nil || len(runs) != 1 || len(phases) != 2 || api.creations != 1 {
				t.Fatalf("duplicate recovery work: runs=%v, phases=%v, PRs=%d, err=%v", runs, phases, api.creations, err)
			}
			if got := gitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7"); got != "1" {
				t.Fatalf("recovery commits = %s", got)
			}
			data, err := os.ReadFile(filepath.Join(phases[0], "result.json"))
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(data, &result); err != nil || result.Status != "success" {
				t.Fatalf("recovered result: %s, %v", data, err)
			}
		})
	}
}
