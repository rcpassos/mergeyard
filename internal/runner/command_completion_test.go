package runner_test

import (
	"context"
	"errors"
	"github.com/rcpassos/mergeyard/internal/runner"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The foreground waits until nohup has installed HUP ignore and the child has
// started. Without this handshake, pane exit can kill the child before exec.
const synchronizedBackgroundScript = `/usr/bin/nohup /bin/sh -c 'printf ready > "$CHILD_READY"; /bin/sleep 60' >/dev/null 2>&1 &
while [ ! -f "$CHILD_READY" ]; do /bin/sleep 0.01; done`

func assertBackgroundChildReady(t *testing.T, r runner.Runner, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		marker, err := r.ReadFile(context.Background(), path)
		if err == nil && string(marker) == "ready" {
			return
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owned background readiness: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("owned background child never reached readiness")
}

func TestTimedCommandsBoundDescendantsAfterForegroundExitAndRestart(t *testing.T) {
	r, options := localSessions(t)
	req := phaseRequest(t, "exit 0")
	req.Phase = "setup"
	req.Timeout = time.Second
	req.Command.Executable = "/bin/bash"
	req.Command.Args = []string{"-c", synchronizedBackgroundScript}
	ready := filepath.Join(req.Command.Dir, "child-ready")
	req.Command.Env = map[string]string{"CHILD_READY": ready}
	ref, err := r.StartSession(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.StopSession(context.Background(), ref); err != nil {
			t.Errorf("owned session cleanup: %v", err)
		}
		status, err := r.SessionStatus(context.Background(), ref)
		if err != nil || status.State == runner.SessionRunning {
			t.Errorf("owned group survived cleanup: %+v %v", status, err)
		}
	})
	// No control-plane observations or cleanup run during the deadline.
	assertBackgroundChildReady(t, r, ready)
	time.Sleep(1300 * time.Millisecond)
	restarted := runner.NewLocal(options)
	status, err := restarted.SessionStatus(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if status.State == runner.SessionRunning {
		t.Fatalf("owned descendants outlived configured deadline after foreground exit: %+v", status)
	}
}

func TestTimedShortCommandCompletesBeforeDeadline(t *testing.T) {
	r, _ := localSessions(t)
	req := phaseRequest(t, "exit 0")
	req.Phase = "checks"
	// awaitExit allows five seconds for process observation, well before this
	// deadline, without making host startup latency part of the assertion.
	req.Timeout = 30 * time.Second
	ref, err := r.StartSession(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	status := awaitExit(t, r, ref)
	if status.ExitCode == nil || *status.ExitCode != 0 || status.TimedOut {
		t.Fatalf("short command waited for timeout: %+v", status)
	}
}

func TestUntimedAgentStillRetainsOwnedBackgroundWork(t *testing.T) {
	r, _ := localSessions(t)
	req := phaseRequest(t, synchronizedBackgroundScript)
	ready := filepath.Join(req.Command.Dir, "child-ready")
	req.Command.Env = map[string]string{"CHILD_READY": ready}
	ref, err := r.StartSession(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.StopSession(context.Background(), ref); err != nil {
			t.Errorf("owned session cleanup: %v", err)
		}
		status, err := r.SessionStatus(context.Background(), ref)
		if err != nil || status.State == runner.SessionRunning {
			t.Errorf("owned group survived cleanup: %+v %v", status, err)
		}
	})
	assertBackgroundChildReady(t, r, ready)
	time.Sleep(100 * time.Millisecond)
	status, err := r.SessionStatus(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != runner.SessionRunning {
		t.Fatalf("untimed agent background behavior changed: %+v", status)
	}
}
