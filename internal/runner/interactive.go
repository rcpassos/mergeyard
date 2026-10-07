package runner

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/rcpassos/mergeyard/internal/fault"
)

// RunInteractive attaches normal terminal streams and environment to an exact
// resume invocation. The run's advisory lock excludes concurrent CLI resumes;
// unlike the runtime lock, it does not own or write lifecycle state.
func RunInteractive(ctx context.Context, root, runID string, command ExecRequest, stdin io.Reader, stdout, stderr io.Writer) error {
	if runID == "" || runID == "." || runID == ".." || strings.ContainsAny(runID, "/\\\x00") {
		return &fault.Error{Code: "takeover.invalid_run", Message: "Interactive takeover requires a safe run ID"}
	}
	physicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return localFailure("takeover.lock_failed", root, err)
	}
	runDir, err := filepath.EvalSymlinks(filepath.Join(root, "runs", runID))
	if err != nil {
		return localFailure("takeover.lock_failed", runID, err)
	}
	relative, err := filepath.Rel(physicalRoot, runDir)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return &fault.Error{Code: "takeover.lock_failed", Message: "Interactive ownership directory is outside the runtime workspace", Err: err}
	}
	lock, err := os.OpenFile(filepath.Join(runDir, "interactive.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return localFailure("takeover.lock_failed", runID, err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return &fault.Error{Code: "takeover.interactive_running", Message: "An interactive takeover is already running for this run; exit it before resuming again"}
		}
		return localFailure("takeover.lock_failed", runID, err)
	}
	cmd := exec.CommandContext(ctx, command.Executable, command.Args...)
	cmd.Dir = command.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	// Keep the lock in the child as well, including if the CLI parent is killed.
	cmd.ExtraFiles = []*os.File{lock}
	if err := cmd.Run(); err != nil {
		return &fault.Error{Code: "takeover.launch_failed", Message: "Interactive resume failed; the run remains MANUAL. Retry takeover to resume the same conversation", Err: err}
	}
	return nil
}
