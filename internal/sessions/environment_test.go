package sessions

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestEnvironmentCleanupAfterFailedLaunchAndReconciliation(t *testing.T) {
	// A disconnected server proves absence, without starting a real session.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\nprintf \"can't find session\\n\" >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	for _, scenario := range []string{"launch-failure", "exited", "missing", "stop-cleanup-error"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			manager := New(Options{})
			command := Command{Executable: "/bin/sh", Dir: dir, Env: map[string]string{"SECRET": "plain-sentinel"}}
			if scenario == "stop-cleanup-error" {
				// A nonempty directory gives a portable unlink failure, including
				// when the test runs as root and chmod would not deny removal.
				if err := os.Mkdir(environmentPath(dir), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(environmentPath(dir), "secret"), []byte("plain-sentinel"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := RequireProcessJournal(dir); err != nil {
					t.Fatal(err)
				}
				err := manager.Stop(context.Background(), Ref{Name: "test", PhaseDir: dir})
				assertCode(t, err, "phase.environment_failed")
				return
			}
			if scenario == "launch-failure" {
				_, err := manager.Start(context.Background(), Request{RunID: "test", Phase: "implement", Attempt: 1, PhaseDir: dir, Command: command})
				assertCode(t, err, "phase.session_start_failed")
			} else {
				// Model a control-plane crash before Start's deferred cleanup.
				if err := writeEnvironment(command, environmentPath(dir)); err != nil {
					t.Fatal(err)
				}
				want := Missing
				if scenario == "exited" {
					want = Exited
					if err := os.WriteFile(filepath.Join(dir, "exit.json"), []byte(`{"exit_code":1}`), 0600); err != nil {
						t.Fatal(err)
					}
				}
				status, err := manager.Status(context.Background(), Ref{Name: "test", PhaseDir: dir})
				if err != nil || status.State != want {
					t.Fatalf("status=%+v, err=%v", status, err)
				}
			}
			if _, err := os.Stat(environmentPath(dir)); !os.IsNotExist(err) {
				t.Fatalf("environment file survived %s: %v", scenario, err)
			}
		})
	}
}
