package scheduler_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

type ciProcessInput struct {
	Config                  config.Config
	Workspace, ExternalPath string
	PR                      *github.PullRequest
	Now                     time.Time
	DuringReady             bool
}
type interruptedReadyGitHub struct {
	*ciGitHub
	path string
}

func (g interruptedReadyGitHub) MarkReady(_ context.Context, _ string, _ int) error {
	pr := g.prs["mergeyard/issue-7"]
	pr.Draft = false
	data, err := json.Marshal(pr)
	if err != nil {
		return err
	}
	if err := os.WriteFile(g.path, data, 0600); err != nil {
		return err
	}
	fmt.Println("ci-control-plane-ready")
	select {} // Parent kills after the remote write, before its local acknowledgement.
}
func TestCIControlPlaneProcess(t *testing.T) {
	path := os.Getenv("MERGEYARD_TEST_CI_PROCESS")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var input ciProcessInput
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	runtime, err := app.Open(context.Background(), input.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	api := &ciGitHub{fakeGitHub: &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}, prs: map[string]*github.PullRequest{"mergeyard/issue-7": input.PR}}, evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "in_progress"}}}}
	var boundary scheduler.GitHub = api
	if input.DuringReady {
		api.evidence.Checks[0].Status = "completed"
		api.evidence.Checks[0].Conclusion = "success"
		boundary = interruptedReadyGitHub{api, input.ExternalPath}
	}
	s, err := scheduler.New(input.Config, schedulerResources(runtime), scheduler.Dependencies{GitHub: boundary, Now: func() time.Time { return input.Now }})
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
	for _, mode := range []string{"CI wait", "mark ready applied"} {
		t.Run(mode, func(t *testing.T) {
			_, initial, base, _, cfg, r := localFlow(t, successfulScript)
			now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
			api := &ciGitHub{fakeGitHub: base, applyReady: true, evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "in_progress"}}}}
			s, err := scheduler.New(cfg, schedulerResources(initial), scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			run := finish(t, s, workflow.WaitingForCI, workflow.Review)
			root := initial.Workspace.Root
			if err := initial.Close(); err != nil {
				t.Fatal(err)
			}
			external := filepath.Join(t.TempDir(), "remote-pr.json")
			input, _ := json.Marshal(ciProcessInput{Config: cfg, Workspace: root, ExternalPath: external, PR: base.prs["mergeyard/issue-7"], Now: now.Add(time.Minute), DuringReady: mode == "mark ready applied"})
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
				api.prs["mergeyard/issue-7"] = &pr
				api.evidence.Checks[0].Status = "completed"
				api.evidence.Checks[0].Conclusion = "success"
			}
			now = now.Add(2 * time.Minute)
			cfg.CITimeout = time.Minute
			s, err = scheduler.New(cfg, schedulerResources(reopened), scheduler.Dependencies{GitHub: api, Now: func() time.Time { return now }})
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
			if saved.State != want || !saved.CI.Deadline.Equal(run.CI.Deadline) || api.readyCalls != 0 {
				t.Fatalf("restart state %+v %+v writes=%d", saved, saved.CI, api.readyCalls)
			}
		})
	}
}
