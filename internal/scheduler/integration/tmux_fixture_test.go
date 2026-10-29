package integration_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
)

func TestTmuxFixtureCleanupUsesLaunchedAndAdoptedReferences(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux required")
	}
	for _, mode := range []string{"launched", "adopted"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			// Deliberately unrelated to the production run/phase directory layout.
			phaseDir := filepath.Join(root, "relocated", "session-journal")
			if err := os.MkdirAll(phaseDir, 0700); err != nil {
				t.Fatal(err)
			}
			socket := fmt.Sprintf("mergeyard-recorded-cleanup-%d", time.Now().UnixNano())
			options := runner.Options{SocketName: socket}
			raw := runner.NewLocal(options)
			var ref runner.SessionRef
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if ref.Name != "" {
					_ = raw.StopSession(ctx, ref)
				}
				_ = exec.CommandContext(ctx, "tmux", "-L", socket, "kill-server").Run()
			})
			t.Run("controller", func(t *testing.T) {
				owned := testkit.NewLocalRunner(t, options)
				var launcher runner.Runner = owned
				if mode == "adopted" {
					launcher = raw
				}
				var err error
				ref, err = launcher.StartSession(t.Context(), runner.SessionRequest{RunID: "owned", Phase: "fixture", Attempt: 1, PhaseDir: phaseDir, Command: runner.ExecRequest{Executable: "/bin/sh", Args: []string{"-c", "printf ready > started; exec /bin/sleep 60"}, Dir: root}})
				if err != nil {
					t.Fatal(err)
				}
				testkit.WaitFor(t, func() bool { _, err := os.Stat(filepath.Join(root, "started")); return err == nil })
				if mode == "adopted" {
					status, err := owned.SessionStatus(t.Context(), ref)
					if err != nil || status.State != runner.SessionRunning {
						t.Fatalf("adoption: %+v, %v", status, err)
					}
				}
			})
			status, err := raw.SessionStatus(t.Context(), ref)
			if err != nil || status.State == runner.SessionRunning {
				t.Fatalf("cleanup missed %s session outside production layout: %+v, %v", mode, status, err)
			}
		})
	}
}
