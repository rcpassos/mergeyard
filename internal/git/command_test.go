package git_test

import (
	"context"
	"errors"
	"fmt"
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
	return managedgit.Timeouts{Metadata: timeout, Worktree: timeout, Clone: timeout, Fetch: timeout, Push: timeout}
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
// the context deadline. It must run after any real Git setup. Unless the body
// answers it, the script resolves the SSH policy's configuration probe with the
// real Git so a hanging fake does not stall that read.
func installFakeGit(t *testing.T, body string) {
	t.Helper()
	if !strings.Contains(body, "config --get core.sshCommand") {
		if real, err := exec.LookPath("git"); err == nil {
			body = `case " $* " in *" config --get core.sshCommand "*) exec ` + real + ` "$@";; esac
` + body
		}
	}
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
	if !strings.Contains(err.Error(), "clone timed out") || !strings.Contains(err.Error(), "clone budget") {
		t.Fatalf("timeout does not explain the clone budget: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("hung command held the manager for %s", elapsed)
	}
}

func TestInitialCloneKeepsItsOwnBudget(t *testing.T) {
	f := newFixture(t)
	gitPath := realGit(t)
	// The first clone is slower than the routine fetch and worktree budgets.
	installFakeGit(t, `case " $* " in *" clone "*) /bin/sleep 2;; esac
exec `+gitPath+` "$@"`)
	manager := managerWithTimeouts(t, f, managedgit.Timeouts{
		Metadata: 5 * time.Second, Worktree: 5 * time.Second,
		Clone: 10 * time.Second, Fetch: time.Second, Push: 5 * time.Second,
	})
	run, err := manager.Prepare(context.Background(), f.request)
	if err != nil {
		t.Fatalf("first clone did not get its own budget: %v", err)
	}
	if _, err := os.Stat(run.BasePath); err != nil {
		t.Fatalf("clone did not publish the managed base: %v", err)
	}
}

func TestInterruptedCloneRestartsInsteadOfBecomingAFetch(t *testing.T) {
	f := newFixture(t)
	gitPath := realGit(t)
	// The test controls whether the clone hangs, so the first attempt is
	// interrupted and the second one can complete.
	hang := filepath.Join(t.TempDir(), "hang-clone")
	if err := os.WriteFile(hang, nil, 0600); err != nil {
		t.Fatal(err)
	}
	installFakeGit(t, `case " $* " in
  *" clone "*)
    if [ -f "`+hang+`" ]; then exec /bin/sleep 600; fi;;
esac
exec `+gitPath+` "$@"`)
	manager := managerWithTimeouts(t, f, managedgit.Timeouts{
		Metadata: 5 * time.Second, Worktree: 5 * time.Second,
		Clone: time.Second, Fetch: 5 * time.Second, Push: 5 * time.Second,
	})
	_, err := manager.Prepare(context.Background(), f.request)
	requireCode(t, err, "git.timeout")
	if err := os.Remove(hang); err != nil {
		t.Fatal(err)
	}
	// The killed transfer never became a managed base, so the next attempt
	// cannot mistake it for a completed clone and start the transfer as a fetch.
	base := filepath.Join(f.root, "repos", "owner-repo", "base")
	if _, err := os.Stat(base); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted clone left a managed base: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(base), ".base-clone")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted clone left staging data: %v", err)
	}
	run, err := manager.Prepare(context.Background(), f.request)
	if err != nil {
		t.Fatalf("next attempt did not restart the clone: %v", err)
	}
	if _, err := os.Stat(run.BasePath); err != nil {
		t.Fatalf("restarted clone did not publish the managed base: %v", err)
	}
}

func TestWorktreeCreationKeepsItsOwnBudget(t *testing.T) {
	f := newFixture(t)
	// Prepare the shared base first so the slow command applies only to the run
	// worktree's creation.
	setup := f.request
	setup.RunID, setup.IssueNumber = "setup", 6
	if _, err := f.manager.Prepare(context.Background(), setup); err != nil {
		t.Fatal(err)
	}
	gitPath := realGit(t)
	installFakeGit(t, `case " $* " in *" worktree add "*) /bin/sleep 1;; esac
exec `+gitPath+` "$@"`)
	manager := managerWithTimeouts(t, f, managedgit.Timeouts{
		Metadata: 5 * time.Second, Worktree: 5 * time.Second,
		Clone: 5 * time.Second, Fetch: time.Second, Push: 5 * time.Second,
	})
	run, err := manager.Prepare(context.Background(), f.request)
	if err != nil {
		t.Fatalf("worktree creation did not get its own budget: %v", err)
	}
	if _, err := os.Stat(run.Path); err != nil {
		t.Fatalf("worktree was not created: %v", err)
	}
}

func TestWorktreeCreationStartsWithAFullBudget(t *testing.T) {
	f := newFixture(t)
	// Prepare the shared base first so the slow commands apply only to the run.
	setup := f.request
	setup.RunID, setup.IssueNumber = "setup", 6
	if _, err := f.manager.Prepare(context.Background(), setup); err != nil {
		t.Fatal(err)
	}
	gitPath := realGit(t)
	// Checkout and worktree creation each fit the worktree budget, but together
	// they would exceed it if they shared one deadline.
	installFakeGit(t, `case " $* " in
  *" checkout "*|*" worktree add "*) /bin/sleep 1.5;;
esac
exec `+gitPath+` "$@"`)
	manager := managerWithTimeouts(t, f, managedgit.Timeouts{
		Metadata: 5 * time.Second, Worktree: 2 * time.Second,
		Clone: 5 * time.Second, Fetch: 5 * time.Second, Push: 5 * time.Second,
	})
	run, err := manager.Prepare(context.Background(), f.request)
	if err != nil {
		t.Fatalf("worktree creation shared the checkout's deadline: %v", err)
	}
	if _, err := os.Stat(run.Path); err != nil {
		t.Fatalf("worktree was not created: %v", err)
	}
}

func TestPrePushHookKeepsItsOwnBudget(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	write(t, run.Path, "feature.txt", "implemented\n")
	hooks := t.TempDir()
	if err := os.WriteFile(filepath.Join(hooks, "pre-push"), []byte("#!/bin/sh\n/bin/sleep 2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	git(t, run.Path, "config", "core.hooksPath", hooks)
	// The hook outlives the routine fetch budget but must fit the push budget.
	manager := managerWithTimeouts(t, f, managedgit.Timeouts{
		Metadata: 5 * time.Second, Worktree: 5 * time.Second,
		Clone: 5 * time.Second, Fetch: time.Second, Push: 10 * time.Second,
	})
	result, err := manager.CommitAndPush(context.Background(), run, managedgit.Phase{Title: "slow hook", RequireChanges: true})
	if err != nil {
		t.Fatalf("pre-push hook did not get the push budget: %v", err)
	}
	if got := git(t, f.remote, "rev-parse", "refs/heads/"+run.Branch); got != result.SHA {
		t.Fatalf("push landed %s, want %s", got, result.SHA)
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
	if !strings.Contains(err.Error(), "before its clone budget") {
		t.Fatalf("caller deadline was attributed to the class budget: %v", err)
	}
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
	manager := managerWithTimeouts(t, f, managedgit.Timeouts{Metadata: 30 * time.Second, Worktree: 500 * time.Millisecond, Clone: 30 * time.Second, Fetch: 30 * time.Second, Push: 30 * time.Second})
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

func TestConfiguredCoreSSHCommandIsPreserved(t *testing.T) {
	f := newFixture(t)
	// A distinct configuration source so the SSH policy resolves it fresh.
	config := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(config, []byte("[core]\n\tsshCommand = ssh -i from-config\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_SSH", "")
	t.Setenv("GIT_SSH_COMMAND", "")
	dump := filepath.Join(t.TempDir(), "environment")
	gitPath := realGit(t)
	installFakeGit(t, `case " $* " in *" config --get core.sshCommand "*) exec `+gitPath+` "$@";; esac
{ printf 'GIT_SSH_COMMAND=%s\n' "$GIT_SSH_COMMAND"; } > `+dump+`
exit 1`)
	manager := managerWithTimeouts(t, f, shortTimeouts(time.Second))
	_, prepareErr := manager.Prepare(context.Background(), f.request)
	if prepareErr == nil {
		t.Fatal("expected the fake Git to fail")
	}
	data, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("%v (prepare: %v)", err, prepareErr)
	}
	if got := strings.TrimSpace(string(data)); got != "GIT_SSH_COMMAND=" {
		t.Fatalf("managed default replaced a configured core.sshCommand: %q", got)
	}
}

func TestPublicationKeepsAPhaseBudgetPerStep(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	write(t, run.Path, "feature.txt", "implemented\n")
	gitPath := realGit(t)
	// The commit and the push together exceed either class budget; each phase
	// must still keep its own deadline.
	installFakeGit(t, `case " $* " in
  *" commit "*) /bin/sleep 2; exec `+gitPath+` "$@";;
  *" push "*) /bin/sleep 2; exec `+gitPath+` "$@";;
esac
exec `+gitPath+` "$@"`)
	manager := managerWithTimeouts(t, f, managedgit.Timeouts{Metadata: 30 * time.Second, Worktree: 3 * time.Second, Clone: 30 * time.Second, Fetch: 3 * time.Second, Push: 3 * time.Second})
	result, err := manager.CommitAndPush(context.Background(), run, managedgit.Phase{Title: "two phases", RequireChanges: true})
	if err != nil {
		t.Fatalf("commit and push shared one budget: %v", err)
	}
	if !result.Committed || result.SHA == "" {
		t.Fatalf("result = %+v", result)
	}
	if got := git(t, f.remote, "rev-parse", "refs/heads/"+run.Branch); got != result.SHA {
		t.Fatalf("push landed %s, want %s", got, result.SHA)
	}
}

func TestConditionalIncludeSSHCommandIsPreserved(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	// Git matches gitdir conditions against the resolved repository path.
	repo, err := filepath.EvalSymlinks(run.BasePath)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	include := filepath.Join(dir, "included.gitconfig")
	if err := os.WriteFile(include, []byte("[core]\n\tsshCommand = ssh -i from-include\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(config, []byte(fmt.Sprintf("[includeIf \"gitdir:%s/\"]\n\tpath = %s\n", repo, include)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_SSH", "")
	t.Setenv("GIT_SSH_COMMAND", "")
	dump := filepath.Join(dir, "environment")
	gitPath := realGit(t)
	installFakeGit(t, `case " $* " in *" config --get core.sshCommand "*) exec `+gitPath+` "$@";; esac
{ printf 'GIT_SSH_COMMAND=%s\n' "$GIT_SSH_COMMAND"; } > `+dump+`
exit 1`)
	manager := managerWithTimeouts(t, f, shortTimeouts(time.Second))
	// Inspect reads the base repository, so the probe resolves the include
	// that selects its transport.
	if err := manager.Inspect(context.Background(), run); err == nil {
		t.Fatal("expected the fake Git to fail")
	}
	data, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "GIT_SSH_COMMAND=" {
		t.Fatalf("managed default replaced a conditionally included core.sshCommand: %q", got)
	}
}

func TestPublicationBoundsThePushByItsOwnBudget(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	write(t, run.Path, "feature.txt", "implemented\n")
	gitPath := realGit(t)
	// A fast commit followed by a stalled push: the push must keep the remote
	// budget instead of running under the longer outer publication bound.
	installFakeGit(t, `case " $* " in
  *" push "*) /bin/sleep 30; exec `+gitPath+` "$@";;
esac
exec `+gitPath+` "$@"`)
	manager := managerWithTimeouts(t, f, managedgit.Timeouts{Metadata: 30 * time.Second, Worktree: 10 * time.Second, Clone: 30 * time.Second, Fetch: 10 * time.Second, Push: 3 * time.Second})
	start := time.Now()
	_, err := manager.CommitAndPush(context.Background(), run, managedgit.Phase{Title: "stalled publication", RequireChanges: true})
	requireCode(t, err, "git.timeout")
	if !strings.Contains(err.Error(), "push timed out") || !strings.Contains(err.Error(), "push budget") {
		t.Fatalf("stalled push did not report its own deadline: %v", err)
	}
	// The outer publication bound is 53s here; a push without its own budget
	// would still be running when the test checks.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("push ran %s; the outer bound replaced its push budget", elapsed)
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
