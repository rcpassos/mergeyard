package runner_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/runner"
)

func TestExitedSessionDoesNotWaitForZombieProcess(t *testing.T) {
	// Retain the child's wait status so its process remains an unreaped zombie.
	// Such a process can no longer perform work, though kill(group, 0) succeeds.
	command := exec.Command("/bin/sh", "-c", "exit 0")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { command.Process.Kill(); command.Wait() })
	identity := func(pid int) string {
		c := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
		c.Env = append(os.Environ(), "LC_ALL=C")
		out, err := c.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	data, err := json.Marshal(map[string]any{"pid": command.Process.Pid, "started": identity(command.Process.Pid), "boot": identity(1)})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "process-group.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "exit.json"), []byte(`{"exit_code":0}`), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		state, err := exec.Command("ps", "-p", strconv.Itoa(command.Process.Pid), "-o", "stat=").Output()
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(strings.TrimSpace(string(state)), "Z") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("owned child did not become a zombie")
		}
		time.Sleep(5 * time.Millisecond)
	}
	r, _ := localSessions(t)
	status, err := r.SessionStatus(context.Background(), runner.SessionRef{Name: "ended-session", PhaseDir: dir})
	if err != nil || status.State != runner.SessionExited || status.ExitCode == nil || *status.ExitCode != 0 {
		t.Fatalf("completed execution held by zombie: %+v %v", status, err)
	}
	if err := r.StopSession(context.Background(), runner.SessionRef{Name: "ended-session", PhaseDir: dir}); err != nil {
		t.Fatalf("zombie prevented Stop: %v", err)
	}
}
