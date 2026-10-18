package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/rcpassos/mergeyard/internal/fault"
)

// An unattended Git operation must never wait on a terminal prompt or an
// unresponsive SSH peer. BatchMode fails instead of prompting, ConnectTimeout
// bounds the handshake, and the keepalive options detect a dead connection.
// GIT_SSH and GIT_SSH_COMMAND already present in the environment still win, so
// a machine with a custom transport keeps it.
const nonInteractiveSSH = "ssh -o BatchMode=yes -o ConnectTimeout=15 -o ServerAliveInterval=30 -o ServerAliveCountMax=4"

// Execute Git directly, with the machine's identity and credentials. Disable
// terminal prompts so an unattended run reports failures instead of hanging.
func command(ctx context.Context, dir, code string, args ...string) (string, error) {
	return commandWithEnv(ctx, dir, code, nil, args...)
}

func commandWithEnv(ctx context.Context, dir, code string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"), env...)
	if os.Getenv("GIT_SSH_COMMAND") == "" && os.Getenv("GIT_SSH") == "" {
		cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND="+nonInteractiveSSH)
	}
	// A repository hook or credential helper can outlive Git and keep its
	// output pipes open. Bound the owned process group and the pipe drain so a
	// stalled operation still returns on its deadline.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		} else {
			return err
		}
	}
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return stdout.String(), contextFailure(code, dir, ctx.Err())
	}
	if err != nil {
		return stdout.String(), failure(code, dir, fmt.Errorf("%s: %w", strings.TrimSpace(stderr.String()), err))
	}
	if strings.ContainsRune(stdout.String(), 0) {
		return stdout.String(), nil
	}
	return strings.TrimSpace(stdout.String()), nil
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
