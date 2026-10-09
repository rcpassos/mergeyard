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
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type combinedProcessInput struct {
	Config                                      config.Config
	Workspace, Socket, Blocked, RunID, Boundary string
	Kind                                        harness.FailureKind
	Now                                         time.Time
	Issues                                      map[string][]github.Issue
	PRs                                         map[string]*github.PullRequest
	Remotes                                     map[string]string
}

func TestCombinedControlPlaneProcess(t *testing.T) {
	path := os.Getenv("MERGEYARD_TEST_COMBINED_PROCESS")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var input combinedProcessInput
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	rt, err := app.Open(context.Background(), input.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	api := &fakeGitHub{issues: input.Issues, prs: input.PRs}
	api.head = func(branch string) string {
		return gitCommand(t, input.Remotes[branch], "rev-parse", "refs/heads/"+branch)
	}
	var r runner.Runner = runner.NewLocal(runner.Options{SocketName: input.Socket})
	if input.Boundary == "resume" {
		r = recoveryCrashRunner{Runner: r, phase: workflow.Implement, mode: "after-launch"}
	}
	deps := scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r, Now: func() time.Time { return input.Now }, Harnesses: map[string]harness.HarnessAdapter{input.Blocked: manualLimitHarness(input.Config, input.Blocked, input.Kind)}}
	if input.Boundary == "handback" {
		deps.Git = manualLimitCrashGit{managedgit.New(rt.Workspace), "push"}
	}
	s, err := scheduler.New(input.Config, schedulerResources(rt), deps)
	if err != nil {
		t.Fatal(err)
	}
	switch input.Boundary {
	case "handback":
		if _, err := s.Handback(context.Background(), input.RunID); err != nil {
			t.Fatal(err)
		}
	case "resume":
		for {
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	default:
		if _, err := s.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		fmt.Println("combined-control-plane-ready")
		select {}
	}
	t.Fatal("crash boundary not reached")
}

func killCombinedControlPlane(t *testing.T, input combinedProcessInput) {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "input.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestCombinedControlPlaneProcess$")
	cmd.Env = append(os.Environ(), "MERGEYARD_TEST_COMBINED_PROCESS="+path)
	log, err := os.Create(filepath.Join(t.TempDir(), "child.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd.Stderr = log
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	expected := "combined-control-plane-ready"
	if input.Boundary == "handback" {
		expected = "manual-limit-control-plane-ready"
	} else if input.Boundary == "resume" {
		expected = "review-control-plane-ready"
	}
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != expected {
		diagnostic, _ := os.ReadFile(log.Name())
		t.Fatalf("crash boundary not reached: %q %v\n%s", scanner.Text(), scanner.Err(), diagnostic)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait() // Prove this owned process exited before reopening its workspace.
}
