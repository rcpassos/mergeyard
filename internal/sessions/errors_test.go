package sessions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rcpassos/mergeyard/internal/fault"
)

func assertCode(t *testing.T, err error, want string) {
	t.Helper()
	var failure *fault.Error
	if !errors.As(err, &failure) || failure.Code != want {
		t.Fatalf("error = %v, want code %s", err, want)
	}
}

func TestSessionErrorsExposeStableCodes(t *testing.T) {
	manager := New(Options{})
	_, err := manager.Start(context.Background(), Request{})
	assertCode(t, err, "phase.invalid_request")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "exit.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = manager.Status(context.Background(), Ref{Name: "test", PhaseDir: dir})
	assertCode(t, err, "phase.exit_invalid")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = manager.Status(ctx, Ref{Name: "test", PhaseDir: dir})
	assertCode(t, err, "phase.canceled")
	if !errors.Is(err, context.Canceled) {
		t.Fatal("coded error lost cancellation cause")
	}
	t.Setenv("PATH", t.TempDir())
	_, err = manager.Start(context.Background(), Request{RunID: "test", Phase: "implement", Round: 0, Attempt: 1, PhaseDir: dir, Command: Command{Executable: "/bin/sh", Dir: dir}})
	assertCode(t, err, "phase.tmux_unavailable")
}
