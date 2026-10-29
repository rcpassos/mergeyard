package testkit

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

	"github.com/rcpassos/mergeyard/internal/config"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type UsageProcessInput struct {
	Config                             config.Config
	Workspace, Socket, Agent, Boundary string
	Now                                time.Time
	Credits                            bool
	Check                              bool
	Phase                              workflow.Phase
	PR                                 *github.PullRequest
	Remote                             string
}

type InterruptionCrashHarness struct{ harness.HarnessAdapter }

func (h InterruptionCrashHarness) ClassifyFailure(harness.PhaseArtifacts, time.Time) harness.FailureClassification {
	fmt.Println("review-control-plane-ready")
	select {} // Kill after completed execution evidence, before persisting the limit.
}

type BeforeRestoreGit struct{ *managedgit.Manager }

func (g BeforeRestoreGit) RestoreReview(context.Context, managedgit.Run, managedgit.ReviewSnapshot) error {
	fmt.Println("review-control-plane-ready")
	select {} // The restoration journal is durable but Git remains contaminated.
}

func KillUsageControlPlane(t *testing.T, input UsageProcessInput) {
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
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestUsageLimitControlPlaneProcess$")
	cmd.Env = append(os.Environ(), "MERGEYARD_TEST_USAGE_PROCESS="+path)
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
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "review-control-plane-ready" {
		diagnostic, _ := os.ReadFile(log.Name())
		t.Fatalf("crash boundary not reached: %q %v\n%s", scanner.Text(), scanner.Err(), diagnostic)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait() // Confirm the owned control plane has exited before opening SQLite.
}
