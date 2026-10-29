package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestCombinedControlPlaneProcess(t *testing.T) {

	path := os.Getenv("MERGEYARD_TEST_COMBINED_PROCESS")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var input testkit.CombinedProcessInput
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	rt, err := app.Open(context.Background(), input.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	api := &testkit.FakeGitHub{Issues: input.Issues, Prs: input.PRs}
	api.Head = func(branch string) string {
		return testkit.GitCommand(t, input.Remotes[branch], "rev-parse", "refs/heads/"+branch)
	}
	var r runner.Runner = testkit.NewLocalRunner(t, runner.Options{SocketName: input.Socket})
	if input.Boundary == "resume" {
		r = testkit.RecoveryCrashRunner{Runner: r, Phase: workflow.Implement, Mode: "after-launch"}
	}
	deps := scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r, Now: func() time.Time { return input.Now }, Harnesses: map[string]harness.HarnessAdapter{input.Blocked: testkit.ManualLimitHarness(input.Config, input.Blocked, input.Kind)}}
	if input.Boundary == "handback" {
		deps.Git = testkit.ManualLimitCrashGit{Manager: testkit.TestGitManager(rt.Workspace), Boundary: "push"}
	}
	s, err := testkit.TestScheduler(input.Config, testkit.SchedulerResources(rt), deps)
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
