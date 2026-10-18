package git

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/proc"
)

// NonInteractiveSSH is the SSH command Mergeyard uses whenever it runs Git
// without a terminal: BatchMode fails instead of prompting, ConnectTimeout
// bounds the handshake, and the keepalive options detect a dead connection.
// GIT_SSH, GIT_SSH_COMMAND, and a configured core.sshCommand are the machine's
// explicit transport choice and win over that default.
const NonInteractiveSSH = "ssh -o BatchMode=yes -o ConnectTimeout=15 -o ServerAliveInterval=30 -o ServerAliveCountMax=4"

// sshProbeTimeout bounds the one configuration read that resolves the machine's
// SSH transport choice.
const sshProbeTimeout = 5 * time.Second

// Execute Git directly, with the machine's identity and credentials. Disable
// terminal prompts so an unattended run reports failures instead of hanging.
func command(ctx context.Context, dir, code string, args ...string) (string, error) {
	return commandWithEnv(ctx, dir, code, nil, args...)
}

// sshEnvironment keeps unattended SSH non-interactive without replacing a
// transport the machine already selected. Git ranks GIT_SSH_COMMAND above
// core.sshCommand, so a configured core.sshCommand must be resolved before the
// default is applied.
func sshEnvironment() []string {
	if os.Getenv("GIT_SSH_COMMAND") != "" || os.Getenv("GIT_SSH") != "" {
		return nil
	}
	if configuredSSHCommand() != "" {
		return nil
	}
	return []string{"GIT_SSH_COMMAND=" + NonInteractiveSSH}
}

// sshProbe caches the resolved core.sshCommand per configuration source. The
// environment is read on every call, so a process that changes its
// configuration paths resolves them again.
var sshProbe struct {
	mu    sync.Mutex
	key   string
	value string
	valid bool
}

// configuredSSHCommand reads core.sshCommand from the machine's global and
// system configuration, outside any repository, so one checked-out repository
// cannot answer for the machine.
func configuredSSHCommand() string {
	key := sshConfigKey()
	sshProbe.mu.Lock()
	defer sshProbe.mu.Unlock()
	if sshProbe.valid && sshProbe.key == key {
		return sshProbe.value
	}
	ctx, cancel := context.WithTimeout(context.Background(), sshProbeTimeout)
	defer cancel()
	result, _ := proc.Run(ctx, proc.Request{
		Executable: "git",
		Args:       []string{"config", "--get", "core.sshCommand"},
		Dir:        os.TempDir(),
		Env:        append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"),
	})
	sshProbe.key, sshProbe.value, sshProbe.valid = key, strings.TrimSpace(string(result.Stdout)), true
	return sshProbe.value
}

// sshConfigKey identifies the configuration the probe reads.
func sshConfigKey() string {
	var key strings.Builder
	for _, name := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM", "XDG_CONFIG_HOME", "HOME"} {
		key.WriteString(os.Getenv(name))
		key.WriteByte(0)
	}
	return key.String()
}

func commandWithEnv(ctx context.Context, dir, code string, env []string, args ...string) (string, error) {
	commandEnv := append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"), env...)
	commandEnv = append(commandEnv, sshEnvironment()...)
	result, err := proc.Run(ctx, proc.Request{Executable: "git", Args: args, Dir: dir, Env: commandEnv})
	if ctx.Err() != nil {
		return string(result.Stdout), contextFailure(code, dir, ctx.Err())
	}
	if errors.Is(err, exec.ErrWaitDelay) && result.ExitCode == 0 {
		// Git exited successfully; a hook or helper outlived it holding the
		// output pipes. The command itself completed, and Git wrote its own
		// output before exiting.
		err = nil
	}
	if err != nil {
		return string(result.Stdout), failure(code, dir, fmt.Errorf("%s: %w", strings.TrimSpace(string(result.Stderr)), err))
	}
	stdout := string(result.Stdout)
	if strings.ContainsRune(stdout, 0) {
		return stdout, nil
	}
	return strings.TrimSpace(stdout), nil
}

// contextFailure reports why a managed operation stopped. A deadline is a
// distinct, bounded outcome: it names the operation that stalled so the retry
// and attention paths can report it without leaking command arguments.
func contextFailure(code, dir string, err error) *fault.Error {
	if errors.Is(err, context.DeadlineExceeded) {
		return failure("git.timeout", dir, fmt.Errorf("%s timed out: %w", strings.TrimPrefix(code, "git."), err))
	}
	return failure("git.canceled", dir, err)
}
func refExists(ctx context.Context, dir, ref string) (bool, error) {
	_, err := command(ctx, dir, "git.ref", "show-ref", "--verify", "--quiet", ref)
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if ctx.Err() == nil && errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

func verifyOrigin(ctx context.Context, run Run) (string, error) {
	// Inspect the stored URL before Git applies the machine's insteadOf rules.
	// Transport rewrites remain available for both fetch and push.
	origin, err := command(ctx, run.BasePath, "git.origin_mismatch", "config", "--get", "remote.origin.url")
	if err != nil {
		return "", err
	}
	if origin != run.RemoteURL && (githubRepository(origin) == "" || githubRepository(origin) != githubRepository(run.RemoteURL)) {
		return "", failure("git.origin_mismatch", run.BasePath, errors.New("origin differs from the configured repository"))
	}
	return command(ctx, run.BasePath, "git.origin_mismatch", "remote", "get-url", "origin")
}

// HTTPS and SSH spellings of the same GitHub repository share an identity.
// Other hosts and local mirrors require an exact configured URL match.
func githubRepository(remote string) string {
	if strings.HasPrefix(remote, "git@github.com:") {
		return strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(remote, "git@github.com:"), ".git"))
	}
	u, err := url.Parse(remote)
	if err != nil || !strings.EqualFold(u.Hostname(), "github.com") || (u.Scheme != "https" && u.Scheme != "ssh") || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git"))
}

func hasDiff(ctx context.Context, dir string, args ...string) (bool, error) {
	_, err := command(ctx, dir, "git.diff", args...)
	if err == nil {
		return false, nil
	}
	var exit *exec.ExitError
	if ctx.Err() == nil && errors.As(err, &exit) && exit.ExitCode() == 1 {
		return true, nil
	}
	return false, err
}
