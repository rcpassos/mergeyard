package proc_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/proc"
)

func TestRunCapturesOutputEnvironmentAndExit(t *testing.T) {
	result, err := proc.Run(context.Background(), proc.Request{
		Executable: "/bin/sh",
		Args:       []string{"-c", `printf '%s' "$VALUE"; printf diagnostic >&2; exit 7`},
		Dir:        t.TempDir(),
		Env:        []string{"VALUE=literal ' $() ; value"},
	})
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("err = %v", err)
	}
	if result.ExitCode != 7 || string(result.Stdout) != "literal ' $() ; value" || string(result.Stderr) != "diagnostic" {
		t.Fatalf("result = %+v", result)
	}
}

func TestRunReportsLaunchAndContextFailures(t *testing.T) {
	if _, err := proc.Run(context.Background(), proc.Request{Executable: "/missing/executable"}); err == nil {
		t.Fatal("missing executable was treated as a process exit")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	result, err := proc.Run(ctx, proc.Request{Executable: "/bin/sh", Args: []string{"-c", "sleep 3; printf 'descendant survived'"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("cancellation took %v; descendants held output pipes", elapsed)
	}
	if strings.Contains(string(result.Stdout), "descendant survived") {
		t.Fatalf("descendant survived cancellation: %q", result.Stdout)
	}
}

func TestRunBoundsPipeDrainingAfterTheParentExits(t *testing.T) {
	start := time.Now()
	result, err := proc.Run(context.Background(), proc.Request{Executable: "/bin/sh", Args: []string{"-c", "(sleep 2; echo late) & echo hi"}})
	if !errors.Is(err, exec.ErrWaitDelay) || result.ExitCode != 0 || string(result.Stdout) != "hi\n" {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("pipe draining took %v", elapsed)
	}
}

func TestRunStopsDescendantsAfterAFailedParent(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "survived")
	result, err := proc.Run(context.Background(), proc.Request{Executable: "/bin/sh", Args: []string{"-c", `/bin/sh -c '/bin/sleep 2; : >"$1"' child "$1" & exit 7`, "parent", marker}})
	var exit *exec.ExitError
	if !errors.As(err, &exit) || result.ExitCode != 7 {
		t.Fatalf("parent exit = %+v, err = %v", result, err)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("descendant survived a failed parent: %v", err)
	}
}

func TestRunStopsDescendantsThatOutliveASuccessfulExit(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "survived")
	start := time.Now()
	result, err := proc.Run(context.Background(), proc.Request{Executable: "/bin/sh", Args: []string{"-c", `/bin/sh -c '/bin/sleep 2; : >"$1"' child "$1" & exit 0`, "parent", marker}})
	if !errors.Is(err, exec.ErrWaitDelay) || result.ExitCode != 0 {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("draining took %v", elapsed)
	}
	// The descendant would create the marker 2s after start if it survived.
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("descendant survived a successful exit: %v", err)
	}
}

func TestRunCapturesDescendantOutputBeforeTheDrainBound(t *testing.T) {
	result, err := proc.Run(context.Background(), proc.Request{Executable: "/bin/sh", Args: []string{"-c", "echo hi; (sleep 0.05; echo late) &"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || string(result.Stdout) != "hi\nlate\n" {
		t.Fatalf("complete output = %+v", result)
	}
}
