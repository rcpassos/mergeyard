package git

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"sort"
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

// sshEnvironmentFor keeps unattended SSH non-interactive without replacing a
// transport the machine already selected. Git ranks GIT_SSH_COMMAND above
// core.sshCommand, so a configured core.sshCommand must be resolved before the
// default is applied.
func sshEnvironmentFor(dir string, env []string) []string {
	if environmentValue(env, "GIT_SSH_COMMAND") != "" || environmentValue(env, "GIT_SSH") != "" {
		return nil
	}
	if configuredSSHCommandFor(dir, env) != "" {
		return nil
	}
	return []string{"GIT_SSH_COMMAND=" + NonInteractiveSSH}
}

// sshProbeLimit bounds the probe cache so a long-lived daemon cannot grow it
// without end. Rebuilding an entry costs one bounded configuration read.
const sshProbeLimit = 128

// sshProbe caches the resolved core.sshCommand per directory and configuration
// source. The directory matters: Git evaluates conditional includes such as
// `gitdir:` and `hasconfig:remote.*.url:` only inside a repository, so the
// probe runs where the command runs. An edit to a configuration file is picked
// up on restart.
var sshProbe = struct {
	mu      sync.Mutex
	entries map[string]string
}{entries: map[string]string{}}

// configuredSSHCommandFor reads core.sshCommand for one command directory.
func configuredSSHCommandFor(dir string, env []string) string {
	key := dir + "\x00" + sshConfigKeyFor(env)
	sshProbe.mu.Lock()
	defer sshProbe.mu.Unlock()
	if value, ok := sshProbe.entries[key]; ok {
		return value
	}
	ctx, cancel := context.WithTimeout(context.Background(), sshProbeTimeout)
	defer cancel()
	result, _ := proc.Run(ctx, proc.Request{
		Executable: "git",
		Args:       []string{"config", "--get", "core.sshCommand"},
		Dir:        dir,
		Env:        append(append([]string(nil), env...), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"),
	})
	value := strings.TrimSpace(string(result.Stdout))
	if len(sshProbe.entries) >= sshProbeLimit {
		clear(sshProbe.entries)
	}
	sshProbe.entries[key] = value
	return value
}

// sshConfigKeyFor identifies the configuration the probe reads.
func sshConfigKeyFor(env []string) string {
	names := map[string]bool{"HOME": true, "XDG_CONFIG_HOME": true}
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if name == "GIT_CONFIG" || strings.HasPrefix(name, "GIT_CONFIG_") {
			names[name] = true
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	var key strings.Builder
	for _, name := range ordered {
		key.WriteString(name)
		key.WriteByte(0)
		key.WriteString(environmentValue(env, name))
		key.WriteByte(0)
	}
	return key.String()
}

func commandWithEnv(ctx context.Context, dir, code string, env []string, args ...string) (string, error) {
	out, err := commandBytes(ctx, dir, code, env, args...)
	stdout := string(out)
	if err != nil || strings.ContainsRune(stdout, 0) {
		return stdout, err
	}
	return strings.TrimSpace(stdout), nil
}

// commandBytes runs Git and returns its output exactly as written, for callers
// that compare file contents.
func commandBytes(ctx context.Context, dir, code string, env []string, args ...string) ([]byte, error) {
	return commandPrefix(ctx, dir, code, env, 0, args...)
}

// commandPrefix is commandBytes that keeps at most limit bytes of output, so a
// caller that needs only the beginning of a large file holds only that much.
// A zero limit keeps all of it.
func commandPrefix(ctx context.Context, dir, code string, env []string, limit int, args ...string) ([]byte, error) {
	commandEnv := append(commandEnvironment(ctx), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	commandEnv = append(commandEnv, env...)
	commandEnv = append(commandEnv, sshEnvironmentFor(dir, commandEnv)...)
	result, err := proc.Run(ctx, proc.Request{Executable: "git", Args: args, Dir: dir, Env: commandEnv, StdoutLimit: limit})
	if ctx.Err() != nil {
		return result.Stdout, contextFailure(ctx, code, dir, ctx.Err())
	}
	if errors.Is(err, exec.ErrWaitDelay) && result.ExitCode == 0 {
		// Git exited successfully; a hook or helper outlived it holding the
		// output pipes. The command itself completed, and Git wrote its own
		// output before exiting.
		err = nil
	}
	if err != nil {
		return result.Stdout, failure(code, dir, fmt.Errorf("%s: %w", strings.TrimSpace(string(result.Stderr)), err))
	}
	return result.Stdout, nil
}

type environmentKey struct{}

func commandEnvironment(ctx context.Context) []string {
	env := os.Environ()
	overrides, _ := ctx.Value(environmentKey{}).(map[string]string)
	names := make([]string, 0, len(overrides))
	for name := range overrides {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		env = append(env, name+"="+overrides[name])
	}
	return env
}

// Overrides are appended last; lookup must agree with exec's last-value rule.
func environmentValue(env []string, name string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if value, ok := strings.CutPrefix(env[i], name+"="); ok {
			return value
		}
	}
	return ""
}

// contextFailure reports why a managed operation stopped. A deadline is a
// distinct, bounded outcome: it names the operation and, for a phase deadline,
// the class budget so the dashboard can explain a budget-exhausted failure
// without leaking command arguments.
func contextFailure(ctx context.Context, code, dir string, err error) *fault.Error {
	operation := strings.TrimPrefix(code, "git.")
	if errors.Is(err, context.DeadlineExceeded) {
		message := fmt.Sprintf("%s timed out", operation)
		if b, ok := ctx.Value(budgetKey{}).(budget); ok {
			elapsed := time.Since(b.started).Round(time.Second)
			if elapsed <= 0 {
				elapsed = time.Second
			}
			if elapsed >= b.limit {
				message = fmt.Sprintf("%s timed out after %s; its %s budget is %s", operation, elapsed, b.name, b.limit)
			} else {
				message = fmt.Sprintf("%s timed out after %s, before its %s budget of %s expired", operation, elapsed, b.name, b.limit)
			}
		}
		return &fault.Error{Code: "git.timeout", Path: dir, Message: message, Err: err}
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
	return hasDiffWithEnv(ctx, dir, nil, args...)
}

// hasDiffWithEnv reports whether a command that exits with 1 found
// differences, as Git's diff-shaped commands do.
func hasDiffWithEnv(ctx context.Context, dir string, env []string, args ...string) (bool, error) {
	_, err := commandWithEnv(ctx, dir, "git.diff", env, args...)
	if err == nil {
		return false, nil
	}
	var exit *exec.ExitError
	if ctx.Err() == nil && errors.As(err, &exit) && exit.ExitCode() == 1 {
		return true, nil
	}
	return false, err
}
