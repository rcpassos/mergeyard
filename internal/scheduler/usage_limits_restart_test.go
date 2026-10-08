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
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type usageProcessInput struct {
	Config                             config.Config
	Workspace, Socket, Agent, Boundary string
	Now                                time.Time
}

func TestUsageLimitControlPlaneProcess(t *testing.T) {
	path := os.Getenv("MERGEYARD_TEST_USAGE_PROCESS")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var input usageProcessInput
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	runtime, err := app.Open(context.Background(), input.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}}
	var r runner.Runner = runner.NewLocal(runner.Options{SocketName: input.Socket})
	if input.Boundary == "resuming" {
		r = recoveryCrashRunner{Runner: r, phase: workflow.Implement, mode: "after-launch"}
	}
	adapter := harness.HarnessAdapter(harness.NewClaude(input.Config.Agents.Claude))
	if input.Agent == "codex" {
		adapter = harness.NewCodex(input.Config.Agents.Codex)
	}
	fake := &classifiedHarness{HarnessAdapter: adapter, outcome: harness.FailureClassification{Kind: harness.TemporaryLimit}}
	now := input.Now
	s, err := scheduler.New(input.Config, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{input.Agent: fake}})
	if err != nil {
		t.Fatal(err)
	}
	for {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) == 1 && runs[0].State == workflow.WaitingForHarness {
			if input.Boundary == "waiting" {
				fmt.Println("review-control-plane-ready")
				select {}
			}
			now = runs[0].HarnessWait.ResetAt
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTemporaryLimitSurvivesControlPlaneKill(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		for _, boundary := range []string{"waiting", "resuming"} {
			t.Run(agent+"/"+boundary, func(t *testing.T) {
				_, initial, api, _, cfg, _ := pairingFlow(t, agent, agent, disputedReport)
				script := `case "$*" in *implement-0-1*) printf partial > partial.txt; exit 1;; esac
` + reviewScript(approvedReview)
				replacePhaseScript(t, cfg, workflow.Implement, script)
				root := initial.Workspace.Root
				if err := initial.Close(); err != nil {
					t.Fatal(err)
				}
				socket := fmt.Sprintf("mergeyard-usage-restart-%d", time.Now().UnixNano())
				t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
				now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
				input, _ := json.Marshal(usageProcessInput{Config: cfg, Workspace: root, Socket: socket, Agent: agent, Boundary: boundary, Now: now})
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
				cmd := exec.CommandContext(ctx, executable, "-test.run=^TestUsageLimitControlPlaneProcess$")
				cmd.Env = append(os.Environ(), "MERGEYARD_TEST_USAGE_PROCESS="+path)
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
				runtime, err := app.Open(context.Background(), root)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { runtime.Close() })
				adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
				if agent == "codex" {
					adapter = harness.NewCodex(cfg.Agents.Codex)
				}
				fake := &classifiedHarness{HarnessAdapter: adapter, outcome: harness.FailureClassification{Kind: harness.TemporaryLimit}}
				s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: runner.NewLocal(runner.Options{SocketName: socket}), Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{agent: fake}})
				if err != nil {
					t.Fatal(err)
				}
				runs, err := s.Runs(context.Background())
				if err != nil || len(runs) != 1 || len(runs[0].HarnessWaitHistory) != 1 {
					t.Fatalf("lost waiting journal %+v %v", runs, err)
				}
				waiting := runs[0]
				if boundary == "waiting" {
					for range 3 {
						if _, err := s.Reconcile(context.Background()); err != nil {
							t.Fatal(err)
						}
					}
					saved, err := runtime.Workflow.Get(context.Background(), waiting.ID)
					if err != nil || saved.Implementer.Attempt != 1 {
						t.Fatalf("duplicated waiting attempt %+v %v", saved, err)
					}
				}
				now = waiting.HarnessWait.ResetAt
				done := finish(t, s, workflow.Active, workflow.Review)
				if done.Implementer.Attempt != 2 || done.Implementer.SessionID != waiting.Implementer.SessionID || api.creations != 1 || len(done.HarnessWaitHistory) != 1 {
					t.Fatalf("duplicate launch after kill %+v", done)
				}
			})
		}
	}
}
