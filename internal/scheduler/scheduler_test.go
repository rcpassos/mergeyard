package scheduler_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestBlockedAndUnreadyIssuesAreNeverClaimed(t *testing.T) {
	t.Parallel()

	unready := testkit.Ready(2)
	unready.Labels = nil
	running := testkit.Ready(3)
	running.Labels = append(running.Labels, github.Label{Name: "agent-running"})
	attention := testkit.Ready(4)
	attention.Labels = append(attention.Labels, github.Label{Name: "agent-needs-attention"})
	closed := testkit.Ready(5)
	closed.State = github.Closed
	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(1), unready, running, attention, closed}}, Blockers: map[int][]github.Issue{1: {testkit.Ready(99)}}}
	s, _ := testkit.Fixture(t, api)
	for range 3 {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := s.Runs(context.Background())
	if err != nil || len(runs) != 0 {
		t.Fatalf("ineligible issues created runs: %+v, %v", runs, err)
	}
}

func TestPauseContinuesExistingRunAndResumeClaimsNewIssue(t *testing.T) {
	t.Parallel()

	flow := testkit.NewScenario(t)
	s, runtime, api, _, _, _ := flow.Parts()
	if err := s.Pause(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(context.Background())
	if len(runs) != 0 {
		t.Fatal("paused scheduler claimed an issue")
	}
	if err := s.Pause(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Pause(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	run := flow.Until(s, workflow.Active, workflow.Review)
	// READY_TO_MERGE releases a slot; global pause still blocks new issues.
	for _, trigger := range []workflow.Trigger{workflow.ReviewApproved, workflow.CIPassed} {
		if _, err := runtime.Workflow.Transition(context.Background(), run.ID, workflow.Request{Trigger: trigger}); err != nil {
			t.Fatal(err)
		}
	}
	api.Issues["owner/repo"] = append(api.Issues["owner/repo"], testkit.Ready(8))
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, _ = s.Runs(context.Background())
	if len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatal("paused scheduler claimed another issue")
	}
	if err := s.Pause(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, _ = s.Runs(context.Background())
	if len(runs) != 2 {
		t.Fatal("resume did not claim the newly available issue")
	}
}

func TestOrderingAndGlobalAndRepositoryLimits(t *testing.T) {
	t.Parallel()

	older := testkit.Ready(9)
	older.CreatedAt = older.CreatedAt.Add(-time.Hour)
	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7), older, testkit.Ready(2)}, "owner/second": {testkit.Ready(3), testkit.Ready(1)}, "owner/disabled": {testkit.Ready(4)}}}
	var claims []string
	g := testkit.RejectingGit{PrepareHook: func(req managedgit.PrepareRequest) {
		claims = append(claims, fmt.Sprintf("%s#%d", req.Repository, req.IssueNumber))
	}}
	s, _ := testkit.Configured(t, api, "concurrency: 3\nrepositories:\n  - repo: owner/repo\n    concurrency: 2\n  - repo: owner/second\n  - repo: owner/disabled\n    enabled: false\n", g)
	for range 3 {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(claims, ","); got != "owner/repo#9,owner/repo#2,owner/second#1" {
		t.Fatalf("claim order and limits = %s", got)
	}
	// Attention still consumes capacity even though no workspace was created.
	runs, _ := s.Runs(context.Background())
	if len(runs) != 3 {
		t.Fatalf("active run slots = %d", len(runs))
	}
	for _, run := range runs {
		if run.State != workflow.NeedsAttention || run.LastErrorCode != "git.branch_conflict" {
			t.Fatalf("branch conflict outcome = %+v", run)
		}
	}
}
func TestClaimOrderAndLabelFailureAbortPreparation(t *testing.T) {
	t.Parallel()

	for _, failure := range []string{"", "add", "remove"} {
		t.Run("failure-"+failure, func(t *testing.T) {
			api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}}
			var s *scheduler.Scheduler
			prepared := false
			var actions []string
			api.Mutate = func(action, repo string, n int, label string) error {
				if label == "agent-needs-attention" || (action == "remove" && label == "agent-running") {
					return nil
				}
				runs, err := s.Runs(context.Background())
				if err != nil || len(runs) != 1 || runs[0].State != workflow.Claiming {
					t.Fatalf("labels changed before durable CLAIMING: %+v, %v", runs, err)
				}
				if action == "remove" && !testkit.Has(api.Issues[repo][0], "agent-running") {
					t.Fatal("ready removed before running was added")
				}
				actions = append(actions, action+":"+label)
				if action == failure {
					return &fault.Error{Code: "github.forbidden", Message: "Cannot mutate claim label"}
				}
				return nil
			}
			g := testkit.RejectingGit{PrepareHook: func(req managedgit.PrepareRequest) {
				prepared = true
				runs, _ := s.Runs(context.Background())
				if runs[0].State != workflow.Preparing || testkit.Has(api.Issues[req.Repository][0], "ready-for-agent") || !testkit.Has(api.Issues[req.Repository][0], "agent-running") {
					t.Fatal("preparation began before successful claim")
				}
			}}
			s, _ = testkit.Configured(t, api, "repositories:\n  - repo: owner/repo\n", g)
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if failure != "" && prepared {
				t.Fatal("failed labels allowed preparation")
			}
			if failure == "" && (!prepared || strings.Join(actions, ",") != "add:agent-running,remove:ready-for-agent") {
				t.Fatalf("claim ordering = %v, prepared=%v", actions, prepared)
			}
			runs, _ := s.Runs(context.Background())
			if runs[0].State != workflow.NeedsAttention {
				t.Fatalf("failure outcome = %+v", runs[0])
			}
		})
	}
}

func TestEveryNonterminalStateConsumesSlotExceptReadyToMerge(t *testing.T) {
	t.Parallel()

	for _, state := range []workflow.State{workflow.Claiming, workflow.Preparing, workflow.Active, workflow.Manual, workflow.NeedsAttention, workflow.WaitingForHarness, workflow.WaitingForCI, workflow.ReadyToMerge, workflow.Completed, workflow.Failed, workflow.Stopped} {
		t.Run(string(state), func(t *testing.T) {
			api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}}
			s, runtime := testkit.Configured(t, api, "repositories:\n  - repo: owner/repo\n", testkit.RejectingGit{})
			ctx := context.Background()
			w := runtime.Workflow
			requests := []workflow.Request{{Trigger: workflow.IssueClaimed, Repository: "owner/old", IssueNumber: 1}}
			if state != workflow.Claiming {
				requests = append(requests, workflow.Request{Trigger: workflow.ClaimSucceeded})
			}
			if state != workflow.Claiming && state != workflow.Preparing {
				requests = append(requests, workflow.Request{Trigger: workflow.WorktreeReady})
			}
			fail := &fault.Error{Code: "phase.blocked", Message: "Manual decision needed"}
			switch state {
			case workflow.Manual:
				requests = append(requests, workflow.Request{Trigger: workflow.TakeOver})
			case workflow.NeedsAttention:
				requests = append(requests, workflow.Request{Trigger: workflow.PhaseBlocked, Failure: fail})
			case workflow.WaitingForHarness:
				requests = append(requests, workflow.Request{Trigger: workflow.HarnessLimited})
			case workflow.WaitingForCI, workflow.ReadyToMerge, workflow.Completed:
				requests = append(requests, workflow.Request{Trigger: workflow.ImplementSucceeded}, workflow.Request{Trigger: workflow.ReviewApproved})
				if state != workflow.WaitingForCI {
					requests = append(requests, workflow.Request{Trigger: workflow.CIPassed})
				}
				if state == workflow.Completed {
					requests = append(requests, workflow.Request{Trigger: workflow.PRMerged})
				}
			case workflow.Failed:
				requests = append(requests, workflow.Request{Trigger: workflow.InternalFailure, Failure: fail})
			case workflow.Stopped:
				requests = append(requests, workflow.Request{Trigger: workflow.Stop})
			}
			for _, req := range requests {
				if _, err := w.Transition(ctx, "old-run", req); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			runs, _ := s.Runs(ctx)
			want := 1
			if state == workflow.ReadyToMerge || state.Terminal() {
				want = 2
			}
			if len(runs) != want {
				t.Fatalf("%s slot accounting produced %d runs, want %d", state, len(runs), want)
			}
		})
	}
}

func TestPauseDuringEligibilityStopsPendingClaim(t *testing.T) {
	t.Parallel()

	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}}
	_, runtime := testkit.Fixture(t, api)
	cfg, _, _ := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
	g := testkit.GatedGitHub{FakeGitHub: api, Entered: make(chan struct{}, 1), Release: make(chan struct{})}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: g, Git: testkit.RejectingGit{}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Tick(context.Background()) }()
	select {
	case <-g.Entered:
	case <-time.After(2 * time.Second):
		t.Fatal("tick did not reach blockers")
	}
	if err := s.Pause(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	close(g.Release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(context.Background())
	if len(runs) != 0 {
		t.Fatal("pause returned but pending eligibility check still claimed an issue")
	}
}

func TestPollingRunStopsOnCancellation(t *testing.T) {
	t.Parallel()

	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{}}
	s, _ := testkit.Fixture(t, api)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run cancellation = %v", err)
	}
	if err := s.Tick(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Tick cancellation = %v", err)
	}
}

func TestRunPollsImmediatelyAndPeriodically(t *testing.T) {
	t.Parallel()

	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{}}
	_, runtime := testkit.Fixture(t, api)
	cfg, _, _ := config.Parse([]byte("poll_interval: 10ms\nrepositories:\n  - repo: owner/repo\n"))
	observed := testkit.ObservingGitHub{FakeGitHub: api, Polls: make(chan struct{}, 4)}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: observed})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	for range 2 {
		select {
		case <-observed.Polls:
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not poll")
		}
	}
	if err := s.Run(ctx); err == nil {
		t.Fatal("started concurrent polling loops")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run exit = %v", err)
	}
}

func TestConcurrentTicksProduceOneRunAndAttempt(t *testing.T) {
	t.Parallel()

	flow := testkit.NewScenario(t)
	s, runtime, api, _, _, _ := flow.Parts()
	done := make(chan error, 6)
	for range 6 {
		go func() { done <- s.Tick(context.Background()) }()
	}
	for range 6 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	run := flow.Until(s, workflow.WaitingForCI, workflow.Review)
	runs, _ := s.Runs(context.Background())
	phases, _ := filepath.Glob(filepath.Join(runtime.Workspace.Root, "runs", run.ID, "phases", "*"))
	if len(runs) != 1 || len(phases) != 2 || api.Creations != 1 {
		t.Fatalf("concurrent ticks duplicated work: runs=%d, attempts=%d, PRs=%d", len(runs), len(phases), api.Creations)
	}
}

func TestTakeoverDuringPreparationPreservesManualState(t *testing.T) {
	t.Parallel()

	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}}
	entered := make(chan managedgit.PrepareRequest, 1)
	release := make(chan struct{})
	takeoverDone := make(chan error, 1)
	g := testkit.RejectingGit{PrepareHook: func(req managedgit.PrepareRequest) { entered <- req; <-release }}
	s, runtime := testkit.Configured(t, api, "repositories:\n  - repo: owner/repo\n", g)
	tickDone := make(chan error, 1)
	go func() { tickDone <- s.Tick(context.Background()) }()
	req := <-entered
	go func() {
		_, err := runtime.Workflow.Transition(context.Background(), req.RunID, workflow.Request{Trigger: workflow.TakeOver})
		takeoverDone <- err
	}()
	// A takeover can wait for the in-flight Git operation. If it commits early,
	// the operation must not replace MANUAL with NEEDS_ATTENTION afterwards.
	early := false
	select {
	case err := <-takeoverDone:
		early = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-tickDone; err != nil {
		t.Fatal(err)
	}
	if !early {
		if err := <-takeoverDone; err != nil {
			t.Fatal(err)
		}
	}
	run, err := runtime.Workflow.Get(context.Background(), req.RunID)
	if err != nil || run.State != workflow.Manual {
		t.Fatalf("scheduler overwrote takeover: %+v, %v", run, err)
	}
}

func TestRuntimeDashboardControlSharesDispatchGate(t *testing.T) {
	t.Parallel()

	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}}
	s, runtime := testkit.Configured(t, api, "repositories:\n  - repo: owner/repo\n", testkit.RejectingGit{})
	ctx := context.Background()
	if err := runtime.Scheduler.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(ctx)
	if len(runs) != 0 {
		t.Fatal("dashboard pause did not prevent dispatch")
	}
	if err := s.Pause(ctx, false); err != nil {
		t.Fatal(err)
	}
	if runtime.Scheduler.Paused() {
		t.Fatal("scheduler resume did not update dashboard control")
	}
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, _ = s.Runs(ctx)
	if len(runs) != 1 {
		t.Fatal("shared gate did not resume dispatch")
	}
}
