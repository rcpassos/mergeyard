package testkit

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/sessions"
)

type MergeGitHub struct {
	*CiGitHub
	CloseCalls   int
	CloseError   error
	ApplyClose   bool
	RemoveBudget time.Duration
}

func (f *MergeGitHub) RemoveLabel(ctx context.Context, repo string, n int, label string) error {
	if deadline, ok := ctx.Deadline(); ok {
		f.RemoveBudget = time.Until(deadline)
	}
	return f.CiGitHub.RemoveLabel(ctx, repo, n, label)
}

func (f *MergeGitHub) CloseIssue(_ context.Context, repo string, n int) error {
	f.CloseCalls++
	if f.ApplyClose {
		for i := range f.Issues[repo] {
			if f.Issues[repo][i].Number == n {
				f.Issues[repo][i].State = github.Closed
			}
		}
	}
	return f.CloseError
}

type PendingStopRunner struct {
	runner.Runner
	Pending bool
	Stopped int
}

func (r *PendingStopRunner) StopSession(ctx context.Context, ref runner.SessionRef) error {
	r.Stopped++
	if r.Pending {
		return nil
	}
	return r.Runner.StopSession(ctx, ref)
}

type MergedPublisher struct {
	*MergeGitHub
	Publisher *ReportGitHub
}

func (f *MergedPublisher) EnsureReportComment(ctx context.Context, repo string, n int, body string) (*github.Comment, error) {
	return f.Publisher.EnsureReportComment(ctx, repo, n, body)
}

type UnstartedReviewRunner struct{ runner.Runner }

func (r UnstartedReviewRunner) StartSession(ctx context.Context, req runner.SessionRequest) (runner.SessionRef, error) {
	if req.Phase == "review" {
		return runner.SessionRef{Name: sessions.Name(req), PhaseDir: req.PhaseDir}, fmt.Errorf("interrupted before wrapper launch")
	}
	return r.Runner.StartSession(ctx, req)
}

type FailedPRReadGitHub struct {
	*MergeGitHub
	ReadError    error
	PartialMerge bool
}

func (f *FailedPRReadGitHub) GetPullRequest(ctx context.Context, repo string, n int) (*github.PullRequest, error) {
	if f.ReadError != nil {
		if f.PartialMerge {
			pr, err := f.MergeGitHub.GetPullRequest(ctx, repo, n)
			if err != nil {
				return nil, err
			}
			partial := *pr
			partial.Merged = true
			return &partial, f.ReadError
		}
		return nil, f.ReadError
	}
	return f.MergeGitHub.GetPullRequest(ctx, repo, n)
}

type HeldMergeCleanupGit struct {
	*managedgit.Manager
	Started chan struct{}
	Release chan struct{}
}

func (g *HeldMergeCleanupGit) CleanupWorktree(ctx context.Context, run managedgit.Run) error {
	close(g.Started)
	select {
	case <-g.Release:
		return g.Manager.CleanupWorktree(ctx, run)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// installGitWrapper puts a shell wrapper first in PATH. A matching invocation
// runs body instead of Git and then delegates unless the body exits; every
// other invocation goes straight to the real Git resolved before PATH changed.
func InstallGitWrapper(t *testing.T, match, body string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is unavailable")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \" $* \" in\n*\" " + match + " \"*)\n" + body + "\n;;\nesac\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
