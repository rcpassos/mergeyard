package runner_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/fault"
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

func TestExecCancellationBoundsDescendantPipes(t *testing.T) {
	r := runner.NewLocal(runner.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	result, err := r.Exec(ctx, runner.ExecRequest{Executable: "/bin/sh", Args: []string{"-c", "sleep 3; printf 'descendant survived'"}})
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation = %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("cancellation took %v; descendants held output pipes", elapsed)
	}
	if strings.Contains(string(result.Stdout), "descendant survived") {
		t.Fatalf("descendant survived cancellation: %q", result.Stdout)
	}
}

func TestLocalErrorsHaveCodesAndPreserveCauses(t *testing.T) {
	r := runner.NewLocal(runner.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.Exec(ctx, runner.ExecRequest{Executable: "/bin/sh"})
	var failure *fault.Error
	if !errors.As(err, &failure) || failure.Code != "internal.canceled" || !errors.Is(err, context.Canceled) {
		t.Fatalf("coded cancellation = %v", err)
	}
	_, err = r.Exec(context.Background(), runner.ExecRequest{Executable: "/missing/executable"})
	if !errors.As(err, &failure) || failure.Code != "internal.exec_failed" {
		t.Fatalf("coded launch failure = %v", err)
	}
	_, err = r.ReadFile(context.Background(), filepath.Join(t.TempDir(), "missing"))
	if !errors.As(err, &failure) || failure.Code != "internal.file_read_failed" || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("coded file failure = %v", err)
	}
}

func TestExecBoundsPipeDrainingAfterParentExits(t *testing.T) {
	r := runner.NewLocal(runner.Options{})
	start := time.Now()
	result, err := r.Exec(context.Background(), runner.ExecRequest{Executable: "/bin/sh", Args: []string{"-c", "(sleep 2; echo late) & echo hi"}})
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("unclosed descendant pipes: %v", err)
	}
	var failure *fault.Error
	if !errors.As(err, &failure) || failure.Code != "internal.exec_output_incomplete" {
		t.Fatalf("successful command reported as execution failure: %v", err)
	}
	if result.ExitCode != 0 || string(result.Stdout) != "hi\n" {
		t.Fatalf("successful exit and captured output were lost: %+v", result)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("pipe draining took %v", elapsed)
	}
}

func TestExecCleansDescendantsAfterNonzeroParentExit(t *testing.T) {
	r := runner.NewLocal(runner.Options{})
	marker := filepath.Join(t.TempDir(), "survived")
	result, err := r.Exec(context.Background(), runner.ExecRequest{Executable: "/bin/sh", Args: []string{"-c", `/bin/sh -c '/bin/sleep 2; : >"$1"' child "$1" & exit 7`, "parent", marker}})
	if err != nil || result.ExitCode != 7 {
		t.Fatalf("parent exit = %+v, err=%v", result, err)
	}
	// A surviving child writes the marker after the parent's drain limit.
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("child survived nonzero parent exit: %v", err)
	}
}

func TestExecCapturesDescendantOutputBeforeDrainLimit(t *testing.T) {
	r := runner.NewLocal(runner.Options{})
	result, err := r.Exec(context.Background(), runner.ExecRequest{Executable: "/bin/sh", Args: []string{"-c", "echo hi; (sleep 0.05; echo late) &"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || string(result.Stdout) != "hi\nlate\n" {
		t.Fatalf("complete output = %+v", result)
	}
}
