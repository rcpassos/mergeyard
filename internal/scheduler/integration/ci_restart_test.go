package integration_test

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
	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestCIControlPlaneProcess(t *testing.T) {
	t.Parallel()

	path := os.Getenv("MERGEYARD_TEST_CI_PROCESS")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var input testkit.CiProcessInput
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	runtime, err := app.Open(context.Background(), input.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	api := &testkit.CiGitHub{FakeGitHub: &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}, Prs: map[string]*github.PullRequest{"mergeyard/issue-7": input.PR}}, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "in_progress"}}}}
	var boundary scheduler.GitHub = api
	if input.DuringReady {
		api.Evidence.Checks[0].Status = "completed"
		api.Evidence.Checks[0].Conclusion = "success"
		boundary = testkit.InterruptedReadyGitHub{CiGitHub: api, Path: input.ExternalPath}
	}
	s, err := testkit.TestScheduler(input.Config, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: boundary, Now: func() time.Time { return input.Now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	fmt.Println("ci-control-plane-ready")
	select {}
}
func TestCIRecoversWaitAndReadinessAfterControlPlaneKill(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"CI wait", "mark ready applied"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			_, initial, base, _, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
			now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
			api := &testkit.CiGitHub{FakeGitHub: base, ApplyReady: true, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "in_progress"}}}}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(initial), scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
			root := initial.Workspace.Root
			if err := initial.Close(); err != nil {
				t.Fatal(err)
			}
			external := filepath.Join(t.TempDir(), "remote-pr.json")
			input, _ := json.Marshal(testkit.CiProcessInput{Config: cfg, Workspace: root, ExternalPath: external, PR: base.Prs["mergeyard/issue-7"], Now: now.Add(time.Minute), DuringReady: mode == "mark ready applied"})
			path := filepath.Join(t.TempDir(), "input.json")
			if err := os.WriteFile(path, input, 0600); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, executable, "-test.run=^TestCIControlPlaneProcess$")
			child.Env = append(os.Environ(), "MERGEYARD_TEST_CI_PROCESS="+path)
			stdout, err := child.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			child.Stderr = os.Stderr
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { child.Process.Kill(); child.Wait() })
			scanner := bufio.NewScanner(stdout)
			readySignal := false
			for scanner.Scan() {
				if scanner.Text() == "ci-control-plane-ready" {
					readySignal = true
					break
				}
			}
			if !readySignal {
				t.Fatal("child did not reach persisted CI boundary")
			}
			if err := child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			child.Wait()
			reopened, err := app.Open(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if mode == "mark ready applied" {
				data, err := os.ReadFile(external)
				if err != nil {
					t.Fatal(err)
				}
				var pr github.PullRequest
				if err := json.Unmarshal(data, &pr); err != nil {
					t.Fatal(err)
				}
				api.Prs["mergeyard/issue-7"] = &pr
				api.Evidence.Checks[0].Status = "completed"
				api.Evidence.Checks[0].Conclusion = "success"
			}
			now = now.Add(2 * time.Minute)
			cfg.CITimeout = time.Minute
			s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(reopened), scheduler.Dependencies{GitHub: api, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			saved, err := reopened.Workflow.Get(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := workflow.WaitingForCI
			if mode == "mark ready applied" {
				want = workflow.ReadyToMerge
			}
			if saved.State != want || !saved.CI.Deadline.Equal(run.CI.Deadline) || api.ReadyCalls != 0 {
				t.Fatalf("restart state %+v %+v writes=%d", saved, saved.CI, api.ReadyCalls)
			}
		})
	}
}
