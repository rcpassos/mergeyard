package scheduler_test

import (
	"context"
	"errors"
	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/runner"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestRetryChangedHeadAfterFixUsesReconciledReviewTarget(t *testing.T) {
	_, runtime, base, _, cfg, r := localFlow(t, loopScript(`printf fixed > feature.txt; `+fixedReport))
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	api := &ciGitHub{fakeGitHub: base, evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "queued"}}}}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: ciBranchGitHub{api}, Runner: r, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	before := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if before.ReviewRound != 2 || before.Fix == nil {
		t.Fatalf("fixture did not complete a fix: %+v", before)
	}
	now = before.CI.Deadline
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", before.ID)
	if err := os.WriteFile(filepath.Join(path, "feature.txt"), []byte("published human edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, path, "add", "feature.txt")
	gitCommand(t, path, "commit", "-m", "human edit after round two")
	gitCommand(t, path, "push", "origin", "HEAD")
	target := gitCommand(t, path, "rev-parse", "HEAD")
	selected, err := s.Retry(context.Background(), before.ID)
	if err != nil || selected.State != workflow.Active || selected.Phase != workflow.Review || selected.ApprovedSHA != "" {
		t.Fatalf("retry selection: %+v %v", selected, err)
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	started, err := runtime.Workflow.Get(context.Background(), before.ID)
	if err != nil || started.State != workflow.Active || started.Phase != workflow.Review || started.Review.Attempt != 2 || started.Review.TargetSHA != target {
		t.Fatalf("later review startup rejected reconciled target: state=%s error=%s message=%s review=%+v err=%v", started.State, started.LastErrorCode, started.LastErrorMessage, started.Review, err)
	}
	after := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if after.ApprovedSHA != target || after.ReviewRound != 2 || after.Fix.CommitSHA != before.Fix.CommitSHA || len(after.FixHistory) != 1 {
		t.Fatalf("changed-head retry lost review/fix history: %+v", after)
	}
}

func TestPendingRetryCannotDelayStopDuringPROutage(t *testing.T) {
	for _, mode := range []string{"PR outage", "already stopped", "observed merge", "failed while stopping"} {
		t.Run(mode, func(t *testing.T) {
			_, runtime, base, _, cfg, realRunner := localFlow(t, reviewScript(`/bin/sleep 60`))
			api := &retryStopGitHub{fakeGitHub: base}
			r := &pendingStopRunner{Runner: realRunner, pending: true}
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			before := finish(t, s, workflow.Active, workflow.Review)
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			ref, err := s.Watch(context.Background(), before.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runtime.Workflow.Transition(context.Background(), before.ID, workflow.Request{Trigger: workflow.OperationFailed, Failure: &fault.Error{Code: "internal.test", Message: "Inspect live review"}}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			api.cancel = cancel
			if _, err := s.Retry(ctx, before.ID); !errors.Is(err, context.Canceled) {
				t.Fatalf("fixture did not interrupt Retry: %v", err)
			}
			pending, err := runtime.Workflow.Get(context.Background(), before.ID)
			if err != nil || pending.PendingRetry() == nil {
				t.Fatalf("missing durable retry intent: %+v %v", pending, err)
			}
			api.cancel = nil
			api.offline = true
			if mode == "already stopped" {
				r.pending = false
			}
			stopErr := s.Stop(context.Background(), before.ID)
			if mode == "already stopped" {
				if stopErr != nil {
					t.Fatal(stopErr)
				}
			} else if stopErr == nil {
				t.Fatal("fixture did not leave Stop pending")
			}
			expectedState := workflow.Stopped
			if mode == "failed while stopping" {
				if _, err := runtime.Workflow.Transition(context.Background(), before.ID, workflow.Request{Trigger: workflow.InternalFailure, Failure: &fault.Error{Code: "internal.test", Message: "Runtime failed during Stop"}}); err != nil {
					t.Fatal(err)
				}
				expectedState = workflow.Failed
			}
			if mode == "observed merge" {
				api.offline = false
				api.prs["mergeyard/issue-7"].Merged = true
				api.prs["mergeyard/issue-7"].State = github.Closed
				expectedState = workflow.Completed
			}
			root := runtime.Workspace.Root
			if err := runtime.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := app.Open(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			r.pending = false
			next, err := scheduler.New(cfg, schedulerResources(reopened), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "failed while stopping" {
				r.pending = true
				if _, err := next.Reconcile(context.Background()); err == nil {
					t.Fatal("fixture did not leave terminal interruption pending")
				}
				stillPending, err := reopened.Workflow.Get(context.Background(), before.ID)
				if err != nil || stillPending.PendingRetry() == nil {
					t.Fatalf("Retry intent was retired before terminal process interruption: pending=%+v err=%v", stillPending.PendingRetry(), err)
				}
				r.pending = false
			}
			_, reconcileErr := next.Reconcile(context.Background())
			status, statusErr := realRunner.SessionStatus(context.Background(), ref)
			after, getErr := reopened.Workflow.Get(context.Background(), before.ID)
			if statusErr != nil || status.State == runner.SessionRunning || getErr != nil || after.State != expectedState || after.PendingRetry() != nil {
				t.Fatalf("PR lookup prevented durable Stop recovery: process=%s state=%s pending=%+v reconcile=%v status=%v get=%v", status.State, after.State, after.PendingRetry(), reconcileErr, statusErr, getErr)
			}
			if reconcileErr != nil {
				t.Fatal(reconcileErr)
			}
			if after.Implementer == nil || after.Implementer.Attempt != 1 || len(after.ReviewHistory) != 1 || after.Review.Attempt != 1 {
				t.Fatalf("Stop recovery launched another attempt: %+v", after)
			}
		})
	}
}

type retryStopGitHub struct {
	*fakeGitHub
	cancel  context.CancelFunc
	offline bool
}

func (f *retryStopGitHub) GetPullRequest(ctx context.Context, repo string, n int) (*github.PullRequest, error) {
	if f.cancel != nil {
		f.cancel()
		return nil, ctx.Err()
	}
	if f.offline {
		return nil, errors.New("PR lookup unavailable")
	}
	return f.fakeGitHub.GetPullRequest(ctx, repo, n)
}

func TestRetryBeforePRChecksPublishedBranchAncestry(t *testing.T) {
	for _, state := range []string{"absent", "ancestor", "divergent"} {
		t.Run(state, func(t *testing.T) {
			s, runtime, _, remote, _, _ := localFlow(t, `printf partial > feature.txt; exit 1`)
			before := finish(t, s, workflow.NeedsAttention, workflow.Implement)
			if before.PRNumber != 0 || before.Implementer.Attempt != 1 {
				t.Fatalf("fixture has a PR or extra attempt: %+v", before)
			}
			path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", before.ID)
			gitCommand(t, path, "add", "feature.txt")
			gitCommand(t, path, "commit", "-m", "preserve partial implementation")
			published := ""
			switch state {
			case "ancestor":
				gitCommand(t, path, "push", "origin", "HEAD")
				published = gitCommand(t, path, "rev-parse", "HEAD")
				if err := os.WriteFile(filepath.Join(path, "feature.txt"), []byte("local continuation\n"), 0600); err != nil {
					t.Fatal(err)
				}
				gitCommand(t, path, "add", "feature.txt")
				gitCommand(t, path, "commit", "-m", "continue after published ancestor")
			case "divergent":
				seed := filepath.Join(filepath.Dir(remote), "seed")
				if err := os.WriteFile(filepath.Join(seed, "external.txt"), []byte("external branch\n"), 0600); err != nil {
					t.Fatal(err)
				}
				gitCommand(t, seed, "add", "external.txt")
				gitCommand(t, seed, "commit", "-m", "publish another branch history")
				gitCommand(t, seed, "push", "origin", "HEAD:refs/heads/mergeyard/issue-7")
				published = gitCommand(t, seed, "rev-parse", "HEAD")
			}
			localHead := gitCommand(t, path, "rev-parse", "HEAD")
			preserved, err := os.ReadFile(filepath.Join(path, "feature.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "manual.txt"), []byte("uncommitted human work"), 0600); err != nil {
				t.Fatal(err)
			}
			selected, err := s.Retry(context.Background(), before.ID)
			if state == "divergent" {
				var coded *fault.Error
				if !errors.As(err, &coded) || coded.Code != "retry.head_diverged" || selected.State != workflow.NeedsAttention {
					t.Fatalf("Retry accepted divergent published branch before PR: state=%s err=%v retry=%+v", selected.State, err, selected.Retries)
				}
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			} else if err != nil || selected.State != workflow.Active || selected.Phase != workflow.Implement {
				t.Fatalf("safe unpublished continuation rejected: state=%s err=%v", selected.State, err)
			}
			if got := gitCommand(t, path, "rev-parse", "HEAD"); got != localHead {
				t.Fatalf("Retry rewrote local head: %s", got)
			}
			after, err := os.ReadFile(filepath.Join(path, "feature.txt"))
			if err != nil || string(after) != string(preserved) {
				t.Fatalf("Retry rewrote partial work: %q %v", after, err)
			}
			manual, err := os.ReadFile(filepath.Join(path, "manual.txt"))
			if err != nil || string(manual) != "uncommitted human work" {
				t.Fatalf("Retry lost manual edits: %q %v", manual, err)
			}
			if published != "" && gitCommand(t, remote, "rev-parse", "mergeyard/issue-7") != published {
				t.Fatal("Retry rewrote remote branch")
			}
			current, getErr := runtime.Workflow.Get(context.Background(), before.ID)
			if getErr != nil || current.Implementer == nil || current.Implementer.Attempt != before.Implementer.Attempt {
				t.Fatalf("Retry spent an attempt before reconciliation: %+v %v", current, getErr)
			}
		})
	}
}
