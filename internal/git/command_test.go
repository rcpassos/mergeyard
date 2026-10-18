package git_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/workspace"
)

// shortTimeouts bound every operation class so a never-exiting fake command can
// prove the deadline without a long test.
func shortTimeouts(timeout time.Duration) managedgit.Timeouts {
	return managedgit.Timeouts{Metadata: timeout, Worktree: timeout, Remote: timeout}
}

func managerWithTimeouts(t *testing.T, f fixture, timeouts managedgit.Timeouts) *managedgit.Manager {
	t.Helper()
	w, err := workspace.Open(f.root)
	if err != nil {
		t.Fatal(err)
	}
	return managedgit.NewWithTimeouts(w, timeouts)
}

// installFakeGit replaces the Git executable with a script whose only bound is
// the context deadline. It must run after any real Git setup.
func installFakeGit(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func realGit(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is unavailable")
	}
	return path
}

func TestDeadlineEndsAHungGitCommand(t *testing.T) {
	f := newFixture(t)
	installFakeGit(t, "exec /bin/sleep 600")
	manager := managerWithTimeouts(t, f, shortTimeouts(200*time.Millisecond))
	start := time.Now()
	_, err := manager.Prepare(context.Background(), f.request)
	requireCode(t, err, "git.timeout")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lost the deadline cause: %v", err)
	}
	if !strings.Contains(err.Error(), "clone timed out") {
		t.Fatalf("timeout does not name the operation: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("hung command held the manager for %s", elapsed)
	}
}

func TestCallerDeadlineBoundsAnOperation(t *testing.T) {
	f := newFixture(t)
	installFakeGit(t, "exec /bin/sleep 600")
	// The manager's own budget is generous; the caller's earlier deadline wins.
	manager := managerWithTimeouts(t, f, managedgit.Timeouts{})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := manager.Prepare(ctx, f.request)
	requireCode(t, err, "git.timeout")
}

func TestCallerDeadlineBoundsAManagedWait(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	marker, release := filepath.Join(dir, "started"), filepath.Join(dir, "release")
	// The first operation holds the manager's serialization until this test
	// releases it, however slowly the machine schedules goroutines.
	installFakeGit(t, ": > "+marker+"\nwhile [ ! -f "+release+" ]; do /bin/sleep 0.05; done\nexit 1")
	manager := managerWithTimeouts(t, f, shortTimeouts(30*time.Second))
	holder := make(chan struct{})
	go func() {
		defer close(holder)
		_, _ = manager.Prepare(context.Background(), f.request)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("hung operation never reached its command")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := manager.ListWorktrees(ctx)
	requireCode(t, err, "git.timeout")
	if !strings.Contains(err.Error(), "wait timed out") {
		t.Fatalf("wait failure was not reported as a timeout: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("waited %s for a busy manager", elapsed)
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	<-holder
}

func TestDeadlineEndsAHungRepositoryHook(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	write(t, run.Path, "feature.txt", "implemented\n")
	gitPath := realGit(t)
	// A hook that leaks a background child which keeps Git's output pipes open.
	installFakeGit(t, `case " $* " in *" commit "*) /bin/sleep 600 & wait;; esac
exec `+gitPath+` "$@"`)
	// Verification and staging need a real budget; the hook that hangs is the
	// commit itself, which must still fail on its own deadline.
	manager := managerWithTimeouts(t, f, managedgit.Timeouts{Metadata: 30 * time.Second, Worktree: 500 * time.Millisecond, Remote: 30 * time.Second})
	start := time.Now()
	_, err := manager.CommitAndPush(context.Background(), run, managedgit.Phase{Title: "hung hook", RequireChanges: true})
	requireCode(t, err, "git.timeout")
	if !strings.Contains(err.Error(), "commit timed out") {
		t.Fatalf("timeout does not name the hook operation: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("hung hook held the manager for %s", elapsed)
	}
}

func TestSuccessfulCommandWithLingeringOutputPipesStillCompletes(t *testing.T) {
	f := newFixture(t)
	// ListWorktrees reads registered worktrees from every managed base.
	if err := os.MkdirAll(filepath.Join(f.root, "repos", "owner-repo", "base"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.root, "worktrees"), 0700); err != nil {
		t.Fatal(err)
	}
	// A helper outlives a successful Git exit while holding its output pipes.
	installFakeGit(t, `/bin/sleep 2 &
printf 'worktree /tmp/mergeyard-leaked\0'`)
	manager := managerWithTimeouts(t, f, shortTimeouts(10*time.Second))
	paths, err := manager.ListWorktrees(context.Background())
	if err != nil {
		t.Fatalf("successful command with a lingering helper failed: %v", err)
	}
	found := false
	for _, path := range paths {
		found = found || path == "/tmp/mergeyard-leaked"
	}
	if !found {
		t.Fatalf("git output was not read: %v", paths)
	}
}

func TestSSHOperationsNeverPromptOrHang(t *testing.T) {
	cases := []struct {
		name, configured, want string
	}{
		{"default", "", "GIT_SSH_COMMAND=ssh -o BatchMode=yes"},
		{"existing command wins", "/custom/ssh -i key", "GIT_SSH_COMMAND=/custom/ssh -i key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			dump := filepath.Join(t.TempDir(), "environment")
			installFakeGit(t, `{ printf 'GIT_TERMINAL_PROMPT=%s\n' "$GIT_TERMINAL_PROMPT"
printf 'GIT_SSH_COMMAND=%s\n' "$GIT_SSH_COMMAND"
printf 'GIT_SSH=%s\n' "$GIT_SSH"; } > `+dump+`
exit 1`)
			t.Setenv("GIT_SSH", "")
			t.Setenv("GIT_SSH_COMMAND", tc.configured)
			manager := managerWithTimeouts(t, f, shortTimeouts(time.Second))
			if _, err := manager.Prepare(context.Background(), f.request); err == nil {
				t.Fatal("expected the fake Git to fail")
			}
			data, err := os.ReadFile(dump)
			if err != nil {
				t.Fatal(err)
			}
			environment := string(data)
			if !strings.Contains(environment, "GIT_TERMINAL_PROMPT=0") {
				t.Fatalf("terminal prompting was not disabled: %q", environment)
			}
			if !strings.Contains(environment, tc.want) {
				t.Fatalf("SSH command = %q, want %q", environment, tc.want)
			}
			if tc.configured == "" {
				for _, option := range []string{"ConnectTimeout=15", "ServerAliveInterval=30", "ServerAliveCountMax=4"} {
					if !strings.Contains(environment, option) {
						t.Fatalf("missing dead-connection detection %s: %q", option, environment)
					}
				}
			}
		})
	}
}
