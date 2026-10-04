package harness_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
)

func TestClaudeStreamsAndSurvivesRunnerRestart(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is required for harness session contract tests")
	}
	options := runner.Options{SocketName: fmt.Sprintf("my-harness-%d-%d", os.Getpid(), time.Now().UnixNano()), GracePeriod: 200 * time.Millisecond}
	t.Cleanup(func() { exec.Command("tmux", "-L", options.SocketName, "kill-server").Run() })
	r := runner.NewLocal(options)
	ctx := phase(t)
	ctx.Env = map[string]string{"RELEASE_FILE": filepath.Join(ctx.WorktreePath, "release")}
	adapter := harness.NewClaude(config.Claude{Executable: fakeClaude(t, `printf '%s\n' '{"type":"system","subtype":"init"}'
printf '%s\n' '{"type":"result","is_error":true,"subtype":"error_during_execution"}'
while [ ! -f "$RELEASE_FILE" ]; do /bin/sleep 0.02; done
printf '%s\n' '`+successEvent+`'
printf '%s\n' '{"type":"system","subtype":"notification"}'`)})
	invocation, err := adapter.BuildInvocation(ctx, config.Role{})
	if err != nil {
		t.Fatal(err)
	}
	launch, cancel := context.WithCancel(context.Background())
	defer cancel()
	ref, err := r.StartSession(launch, runner.SessionRequest{RunID: "harness-stream", Phase: "implement", Round: 1, Attempt: 1, PhaseDir: ctx.PhaseDir, Command: invocation})
	if err != nil {
		t.Fatal(err)
	}
	stdoutPath := filepath.Join(ctx.PhaseDir, "events.jsonl")
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, err := r.ReadFile(context.Background(), stdoutPath)
		if err == nil && strings.Contains(string(out), `"subtype":"init"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no streamed output before completion: %s; error = %v", out, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	restarted := runner.NewLocal(options)
	status, err := restarted.SessionStatus(context.Background(), ref)
	if err != nil || status.State != runner.SessionRunning {
		t.Fatalf("long-running attempt after restart = %+v; error = %v", status, err)
	}
	if err := restarted.WriteFile(context.Background(), ctx.Env["RELEASE_FILE"], nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		status, err = restarted.SessionStatus(context.Background(), ref)
		if err != nil {
			t.Fatal(err)
		}
		if status.State == runner.SessionExited {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("attempt did not exit: %+v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.ExitCode == nil {
		t.Fatal("completed attempt has no exit code")
	}
	out, err := restarted.ReadFile(context.Background(), stdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := restarted.ReadFile(context.Background(), filepath.Join(ctx.PhaseDir, "stderr.log"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.ParseResult(ctx, harness.PhaseArtifacts{Stdout: out, Stderr: stderr, ExitCode: *status.ExitCode})
	if err != nil || result.Status != "success" {
		t.Fatalf("last streamed result = %+v; error = %v", result, err)
	}
}
