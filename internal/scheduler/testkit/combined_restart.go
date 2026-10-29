package testkit

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/harness"
)

type CombinedProcessInput struct {
	Config                                      config.Config
	Workspace, Socket, Blocked, RunID, Boundary string
	Kind                                        harness.FailureKind
	Now                                         time.Time
	Issues                                      map[string][]github.Issue
	PRs                                         map[string]*github.PullRequest
	Remotes                                     map[string]string
}

func KillCombinedControlPlane(t *testing.T, input CombinedProcessInput) {
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
