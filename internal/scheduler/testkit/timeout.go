package testkit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rcpassos/mergeyard/internal/github"
)

// heldGitHub blocks discovery so a test can hold the scheduler's tick gate.
type HeldGitHub struct {
	*FakeGitHub
	Entered chan struct{}
	Release chan struct{}
	Once    sync.Once
}

func (h *HeldGitHub) ListLabeledIssues(ctx context.Context, repo string, labels []string) ([]github.Issue, error) {
	h.Once.Do(func() { close(h.Entered) })
	select {
	case <-h.Release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return h.FakeGitHub.ListLabeledIssues(ctx, repo, labels)
}

// installHangingGit replaces the Git executable in PATH. Every command except
// the named one delegates to the real Git, which is resolved first.
func InstallHangingGit(t *testing.T, hang string) {
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
