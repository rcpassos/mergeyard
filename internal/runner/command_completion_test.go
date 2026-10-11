package runner_test

import (
	"context"
	"github.com/rcpassos/mergeyard/internal/runner"
	"testing"
	"time"
)

func TestTimedCommandsBoundDescendantsAfterForegroundExitAndRestart(t *testing.T) {
	r, options := localSessions(t)
	req := phaseRequest(t, "exit 0")
	req.Phase = "setup"
	req.Timeout = time.Second
	req.Command.Executable = "/bin/bash"
	req.Command.Args = []string{"-c", "/usr/bin/nohup /bin/sleep 60 >/dev/null 2>&1 &"}
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
	req := phaseRequest(t, "/usr/bin/nohup /bin/sleep 60 >/dev/null 2>&1 &")
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
	time.Sleep(100 * time.Millisecond)
	status, err := r.SessionStatus(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != runner.SessionRunning {
		t.Fatalf("untimed agent background behavior changed: %+v", status)
	}
}
