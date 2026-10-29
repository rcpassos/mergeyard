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
)

type ManualLimitProcessInput struct {
	Config                                     config.Config
	Workspace, Socket, Remote, RunID, Boundary string
	Now                                        time.Time
}

type ManualLimitCrashGit struct {
	*managedgit.Manager
	Boundary string
}

func (g ManualLimitCrashGit) CommitHandback(ctx context.Context, run managedgit.Run, previous string) (managedgit.CommitResult, error) {
	result, err := g.Manager.CommitHandback(ctx, run, previous)
	if err == nil && g.Boundary == "commit" {
		fmt.Println("manual-limit-control-plane-ready")
		select {}
	}
	return result, err
}

func (g ManualLimitCrashGit) PushHandback(ctx context.Context, run managedgit.Run, previous, target string) error {
	err := g.Manager.PushHandback(ctx, run, previous, target)
	if err == nil && g.Boundary == "push" {
		fmt.Println("manual-limit-control-plane-ready")
		select {}
	}
	return err
}

func KillManualLimitControlPlane(t *testing.T, input ManualLimitProcessInput) {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "manual-limit-process.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestManualLimitControlPlaneProcess$")
	cmd.Env = append(os.Environ(), "MERGEYARD_TEST_MANUAL_LIMIT_PROCESS="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	expected := "manual-limit-control-plane-ready"
	if input.Boundary == "takeover" {
		expected = "review-control-plane-ready"
	}
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != expected {
		t.Fatalf("crash boundary not reached: %q %v", scanner.Text(), scanner.Err())
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
}
