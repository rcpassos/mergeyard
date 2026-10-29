package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestUsageLimitControlPlaneProcess(t *testing.T) {
	t.Parallel()

	path := os.Getenv("MERGEYARD_TEST_USAGE_PROCESS")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var input testkit.UsageProcessInput
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	runtime, err := app.Open(context.Background(), input.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}, Prs: map[string]*github.PullRequest{"mergeyard/issue-7": input.PR}}
	var gh scheduler.GitHub = api
	if input.Remote != "" {
		api.Head = func(branch string) string {
			return testkit.GitCommand(t, input.Remote, "rev-parse", "refs/heads/"+branch)
		}
		gh = testkit.BranchGitHub{FakeGitHub: api}
	}
	phase := input.Phase
	if phase == "" {
		phase = workflow.Implement
	}
	var r runner.Runner = testkit.NewLocalRunner(t, runner.Options{SocketName: input.Socket})
	if input.Boundary == "resuming" {
		r = testkit.RecoveryCrashRunner{Runner: r, Phase: phase, Mode: "after-launch"}
	}
	if input.Boundary == "before-relaunch" {
		r = testkit.RecoveryCrashRunner{Runner: r, Phase: phase, Mode: "before-launch"}
	}
	adapter := harness.HarnessAdapter(harness.NewClaude(input.Config.Agents.Claude))
	if input.Agent == "codex" {
		adapter = harness.NewCodex(input.Config.Agents.Codex)
	}
	fake := &testkit.ClassifiedHarness{HarnessAdapter: adapter, Outcome: harness.FailureClassification{Kind: harness.TemporaryLimit}}
	var h harness.HarnessAdapter = fake
	if input.Credits {
		fake.Outcome.Kind = harness.CreditsExhausted
		h = &testkit.CreditHarness{ClassifiedHarness: fake, Detection: true}
		if input.Boundary == "before-proof" {
			h = testkit.ProofCrashHarness{HarnessAdapter: h}
		}
	}
	if input.Boundary == "interruption" {
		h = testkit.InterruptionCrashHarness{HarnessAdapter: fake}
	}
	var g scheduler.Git = testkit.TestGitManager(runtime.Workspace)
	if input.Boundary == "before-restoration" {
		g = testkit.BeforeRestoreGit{Manager: testkit.TestGitManager(runtime.Workspace)}
	}
	if input.Boundary == "restoration" {
		g = testkit.InterruptedRestoreGit{Manager: testkit.TestGitManager(runtime.Workspace)}
	}
	now := input.Now
	s, err := testkit.TestScheduler(input.Config, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: gh, Git: g, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{input.Agent: h}})
	if err != nil {
		t.Fatal(err)
	}
	if input.Check {
		if input.Boundary == "resuming" {
			r = testkit.HarnessCheckCrashRunner{Runner: r}
		}
		s, err = testkit.TestScheduler(input.Config, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: gh, Git: g, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{input.Agent: h}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CheckHarness(context.Background(), input.Agent); err != nil {
			t.Fatal(err)
		}
		for {
			if _, err := s.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			states, err := s.HarnessAvailability(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range states {
				if v.Harness == input.Agent && v.Check != nil && v.Check.Status == "recovered" && input.Boundary == "after-proof" {
					fmt.Println("review-control-plane-ready")
					select {}
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	for {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if input.Credits && len(runs) == 1 {
			run := runs[0]
			if run.State == workflow.NeedsAttention && run.LastErrorCode == "harness.credits_exhausted" {
				if _, err := s.Retry(context.Background(), run.ID); err != nil {
					t.Fatal(err)
				}
				if input.Boundary == "probe-reserved" {
					fmt.Println("review-control-plane-ready")
					select {}
				}
			}
			if input.Boundary == "after-proof" && run.State == workflow.Active && run.Phase == workflow.Review {
				fmt.Println("review-control-plane-ready")
				select {}
			}
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
	t.Parallel()

	for _, agent := range []string{"claude", "codex"} {
		for _, boundary := range []string{"waiting", "resuming"} {
			t.Run(agent+"/"+boundary, func(t *testing.T) {
				t.Parallel()

				_, initial, api, _, cfg, _ := testkit.TmuxPairingFlow(t, agent, agent, testkit.DisputedReport)
				script := `case "$*" in *implement-0-1*) printf partial > partial.txt; exit 1;; esac
` + testkit.ReviewScript(testkit.ApprovedReview)
				testkit.ReplacePhaseScript(t, cfg, workflow.Implement, script)
				root := initial.Workspace.Root
				if err := initial.Close(); err != nil {
					t.Fatal(err)
				}
				socket := fmt.Sprintf("mergeyard-usage-restart-%d", time.Now().UnixNano())
				t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
				now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
				testkit.KillUsageControlPlane(t, testkit.UsageProcessInput{Config: cfg, Workspace: root, Socket: socket, Agent: agent, Boundary: boundary, Now: now})
				runtime, err := app.Open(context.Background(), root)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { runtime.Close() })
				adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
				if agent == "codex" {
					adapter = harness.NewCodex(cfg.Agents.Codex)
				}
				fake := &testkit.ClassifiedHarness{HarnessAdapter: adapter, Outcome: harness.FailureClassification{Kind: harness.TemporaryLimit}}
				s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: testkit.NewLocalRunner(t, runner.Options{SocketName: socket}), Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{agent: fake}})
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
				done := testkit.Finish(t, s, workflow.Active, workflow.Review)
				if done.Implementer.Attempt != 2 || done.Implementer.SessionID != waiting.Implementer.SessionID || api.Creations != 1 || len(done.HarnessWaitHistory) != 1 {
					t.Fatalf("duplicate launch after kill %+v", done)
				}
			})
		}
	}
}
