package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// processGroup survives tmux/parent exit so stop can still find owned children.
// Boot and leader start identities prevent signaling a reused live leader.
type processGroup struct {
	PID     int    `json:"pid"`
	Started string `json:"started"`
	Boot    string `json:"boot"`
}

func loadProcessGroup(ref Ref) (processGroup, error) {
	var group processGroup
	data, err := os.ReadFile(filepath.Join(ref.PhaseDir, "process-group.json"))
	if err != nil {
		return group, err
	}
	if err = json.Unmarshal(data, &group); err != nil {
		return group, err
	}
	if group.PID <= 1 || strings.TrimSpace(group.Started) == "" || strings.TrimSpace(group.Boot) == "" {
		return group, errors.New("invalid process group identity")
	}
	return group, nil
}
func processStarted(ctx context.Context, pid int) (string, error) {
	cmd := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	var exited *exec.ExitError
	if errors.As(err, &exited) && exited.ExitCode() == 1 && len(out) == 0 {
		return "", nil
	}
	return strings.TrimSpace(string(out)), err
}
func (g processGroup) alive(ctx context.Context) (bool, error) {
	err := syscall.Kill(-g.PID, 0)
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false, err
	}
	// Signal 0 also succeeds for a group containing only unreaped zombies.
	// Observe identities and live members in one snapshot; exited processes
	// cannot perform work and must not hide completed native evidence forever.
	cmd := exec.CommandContext(ctx, "ps", "-axo", "pid=,pgid=,stat=,lstart=")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil {
		return false, err
	}
	var boot, started string
	live := false
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			return false, err
		}
		pgid, err := strconv.Atoi(fields[1])
		if err != nil {
			return false, err
		}
		identity := strings.Join(fields[3:], " ")
		if pid == 1 {
			boot = identity
		}
		if pid == g.PID {
			started = identity
		}
		state := fields[2][0]
		if pgid == g.PID && state != 'Z' && state != 'X' && state != 'x' {
			live = true
		}
	}
	if boot == "" {
		return false, errors.New("cannot establish boot identity; preserve work and inspect before stopping")
	}
	if boot != strings.Join(strings.Fields(g.Boot), " ") || (started != "" && started != strings.Join(strings.Fields(g.Started), " ")) {
		return false, errors.New("process identity changed; preserve work and inspect before stopping")
	}
	return live, nil
}

// RequireProcessJournal records the execution format before a running attempt
// is committed. A missing group journal then proves the harness never launched
// when tmux is absent, because the wrapper journals its group before invocation.
func RequireProcessJournal(phaseDir string) error {
	if err := os.WriteFile(filepath.Join(phaseDir, "process-group-required"), []byte("v1"), 0600); err != nil {
		return failure("phase.process_identity", phaseDir, err)
	}
	return nil
}
func requiresProcessJournal(ref Ref) bool {
	data, err := os.ReadFile(filepath.Join(ref.PhaseDir, "process-group-required"))
	return err == nil && string(data) == "v1"
}
