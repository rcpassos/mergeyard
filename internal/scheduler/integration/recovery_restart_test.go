package integration_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestTmuxMissingSessionRecoverySurvivesControlPlaneKill(t *testing.T) {
	t.Parallel()

	agent := "claude"
	phase := workflow.Fix
	mode := "after-launch"
	t.Run(agent+"/"+string(phase)+"/"+mode, func(t *testing.T) {
		s, initial, api, remote, cfg, _ := testkit.RecoveryFlow(t, agent, phase, "success")

		var run workflow.Run
		// Halt at the failed resume, before the next tick reserves recovery.
		// Preparing the failed fix advances several phases; use the workflow
		// budget rather than the short artifact wait.
		testkit.WaitForWithin(t, 30*time.Second, func() bool {
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			runs, err := s.Runs(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(runs) != 1 || runs[0].Phase != phase {
				return false
			}
			run = runs[0]
			failure := ""

			if run.Fix != nil {
				failure = run.Fix.Error
			}

			return run.State == workflow.Active && strings.HasPrefix(failure, "harness.session_resume_failed:")
		})
		root := initial.Workspace.Root
		if err := initial.Close(); err != nil {
			t.Fatal(err)
		}
		socket := fmt.Sprintf("mergeyard-recovery-kill-%d", time.Now().UnixNano())
		t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
		input, _ := json.Marshal(testkit.ReviewProcessInput{Config: cfg, Workspace: root, Socket: socket, PR: api.Prs["mergeyard/issue-7"], Remote: remote, RecoveryPhase: phase, RecoveryCrash: mode})
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
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestReviewControlPlaneProcess$")
		cmd.Env = append(os.Environ(), "MERGEYARD_TEST_REVIEW_PROCESS="+path)
		cmd.Stderr = os.Stderr
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		scanner := bufio.NewScanner(out)
		if !scanner.Scan() || scanner.Text() != "review-control-plane-ready" {
			t.Fatalf("crash boundary unavailable: %q %v", scanner.Text(), scanner.Err())
		}
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		cmd.Wait()

		runtime, err := app.Open(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		defer runtime.Close()
		persisted, err := runtime.Workflow.Get(context.Background(), run.ID)
		if err != nil || len(persisted.SessionRecoveries) != 1 {
			t.Fatalf("allowance not persisted before launch: %+v %v", persisted, err)
		}

		restarted, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: testkit.NewLocalRunner(t, runner.Options{SocketName: socket})})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := restarted.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		saved := testkit.Finish(t, restarted, workflow.WaitingForCI, workflow.Review)
		if len(saved.SessionRecoveries) != 1 {
			t.Fatal("recovery replenished after crash")
		}

		var attempts int
		if err := runtime.DB.QueryRow("SELECT count(*) FROM phase_attempts WHERE run_id=? AND phase=? AND round=?", run.ID, phase, 1).Scan(&attempts); err != nil {
			t.Fatal(err)
		}
		if attempts != 2 {
			t.Fatalf("duplicate fresh attempt: %d", attempts)
		}
	})

}
