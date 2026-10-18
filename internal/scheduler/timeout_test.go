package scheduler_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// heldGitHub blocks discovery so a test can hold the scheduler's tick gate.
type heldGitHub struct {
	*fakeGitHub
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *heldGitHub) ListLabeledIssues(ctx context.Context, repo string, labels []string) ([]github.Issue, error) {
	h.once.Do(func() { close(h.entered) })
	select {
	case <-h.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return h.fakeGitHub.ListLabeledIssues(ctx, repo, labels)
}

func TestHarnessCheckWaitHonorsCancellation(t *testing.T) {
	_, rt, api, _, cfg, r := localFlow(t, successfulScript)
	held := &heldGitHub{fakeGitHub: api, entered: make(chan struct{}), release: make(chan struct{})}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: held, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	planning := make(chan error, 1)
	go func() {
		_, err := s.Plan(context.Background(), "")
		planning <- err
	}()
	<-held.entered
	// The tick gate is held. Check availability must return on the caller's
	// cancellation instead of waiting for the stalled tick.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	checking := make(chan error, 1)
	go func() {
		_, err := s.CheckHarness(ctx, "claude")
		checking <- err
	}()
	select {
	case err := <-checking:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("harness check error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Check availability ignored cancellation while the tick gate was held")
	}
	close(held.release)
	if err := <-planning; err != nil {
		t.Fatal(err)
	}
}

// installHangingGit replaces the Git executable in PATH. Every command except
// the named one delegates to the real Git, which is resolved first.
func installHangingGit(t *testing.T, hang string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is unavailable")
	}
	dir := t.TempDir()
	script := `#!/bin/sh
case " $* " in *" ` + hang + ` "*) /bin/sleep 600 & wait;; esac
exec ` + real + ` "$@"
`
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestHungGitPublicationTimesOutAndTheNextTickProceeds(t *testing.T) {
	_, rt, api, _, cfg, r := localFlow(t, successfulScript)
	installHangingGit(t, "push")
	// Local preparation and fetch fit the remote budget; the push exhausts it.
	// The budget leaves headroom for slow CI runners while staying far below
	// the test's own deadlines.
	manager := managedgit.NewWithTimeouts(rt.Workspace, managedgit.Timeouts{
		Metadata: 30 * time.Second, Worktree: 30 * time.Second, Remote: 6 * time.Second,
	})
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r, Git: manager})
	if err != nil {
		t.Fatal(err)
	}
	failed := finish(t, s, workflow.NeedsAttention, workflow.Implement)
	if failed.LastErrorCode != "git.timeout" || !strings.Contains(failed.LastErrorMessage, "push timed out") {
		t.Fatalf("hung publication did not end as a coded timeout: %+v", failed)
	}
	if failed.FixHistory != nil || failed.ReviewRound != 0 {
		t.Fatalf("timeout advanced the run: %+v", failed)
	}
	// The hung command released the tick: the next tick reconciles attention.
	start := time.Now()
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("next tick waited %s on the hung Git command", elapsed)
	}
}
