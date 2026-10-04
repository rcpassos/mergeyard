package runner_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/runner"
)

func TestExecCapturesOutputAndExitCode(t *testing.T) {
	r := runner.NewLocal(runner.Options{})
	result, err := r.Exec(context.Background(), runner.ExecRequest{
		Executable: "/bin/sh", Args: []string{"-c", "printf '%s' \"$VALUE\"; printf 'diagnostic' >&2; exit 7"},
		Env: map[string]string{"VALUE": "literal ' $() ; value"}, Dir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 7 || string(result.Stdout) != "literal ' $() ; value" || string(result.Stderr) != "diagnostic" {
		t.Fatalf("unexpected result: %+v", result)
	}
	result, err = r.Exec(context.Background(), runner.ExecRequest{Executable: "/usr/bin/env"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(result.Stdout)) != "" {
		t.Fatalf("inherited environment: %s", result.Stdout)
	}
}

func TestLocalFileOperationsAndCancelledCalls(t *testing.T) {
	r := runner.NewLocal(runner.Options{})
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "input")
	if err := r.WriteFile(ctx, path, []byte("phase input"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := r.ReadFile(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "phase input" {
		t.Fatalf("data = %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %v", info.Mode())
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := r.WriteFile(cancelled, path, []byte("replacement"), 0600); !errors.Is(err, context.Canceled) {
		t.Fatalf("write cancellation = %v", err)
	}
	if _, err := r.ReadFile(cancelled, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("read cancellation = %v", err)
	}
	if err := r.RemovePath(cancelled, dir); !errors.Is(err, context.Canceled) {
		t.Fatalf("remove cancellation = %v", err)
	}
	if _, err := r.Exec(cancelled, runner.ExecRequest{Executable: "/bin/sh", Args: []string{"-c", "touch " + path}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("exec cancellation = %v", err)
	}
	if err := r.RemovePath(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadFile(ctx, path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed file still readable: %v", err)
	}
}

func TestExecReadsInputAndDistinguishesLaunchErrors(t *testing.T) {
	r := runner.NewLocal(runner.Options{})
	path := filepath.Join(t.TempDir(), "input")
	if err := r.WriteFile(context.Background(), path, []byte("provided input"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := r.Exec(context.Background(), runner.ExecRequest{Executable: "/bin/cat", StdinPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || string(result.Stdout) != "provided input" {
		t.Fatalf("result = %+v", result)
	}
	if _, err := r.Exec(context.Background(), runner.ExecRequest{Executable: "/missing/executable"}); err == nil {
		t.Fatal("missing executable was treated as a process exit")
	}
}
