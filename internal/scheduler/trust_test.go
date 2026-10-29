package scheduler_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestUntrustedIssueNeverClaimedAndRefusalVisible(t *testing.T) {
	t.Parallel()

	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}, Trust: func(github.Issue) (github.TrustDecision, error) {
		return github.TrustDecision{Reason: "Label was applied by untrusted actor @outsider."}, nil
	}}
	s, _ := testkit.Fixture(t, api)
	ctx := context.Background()
	for range 3 {
		if err := s.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := s.Runs(ctx)
	if err != nil || len(runs) != 0 {
		t.Fatalf("untrusted issue claimed: %+v %v", runs, err)
	}
	if testkit.Has(api.Issues["owner/repo"][0], "ready-for-agent") || testkit.Has(api.Issues["owner/repo"][0], "agent-running") {
		t.Fatal("refusal must remove ready without adding running")
	}
	if len(api.TrustComments) != 1 || !strings.Contains(api.TrustComments[7], "@outsider") {
		t.Fatalf("refusal comments: %+v", api.TrustComments)
	}
	queue, err := s.Queue(ctx)
	if err != nil || len(queue) != 1 || !queue[0].Attention || !strings.Contains(queue[0].RefusalReason, "@outsider") {
		t.Fatalf("refusal absent from dashboard: %+v %v", queue, err)
	}
}

func TestUnreadableAuthorizationFailsClosed(t *testing.T) {
	t.Parallel()

	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}, Trust: func(github.Issue) (github.TrustDecision, error) {
		return github.TrustDecision{}, &fault.Error{Code: "github.forbidden", Message: "private diagnostic"}
	}}
	s, _ := testkit.Fixture(t, api)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(context.Background())
	if err != nil || len(runs) != 0 {
		t.Fatalf("unverified issue claimed: %+v %v", runs, err)
	}
	if !strings.Contains(api.TrustComments[7], "github.forbidden") || strings.Contains(api.TrustComments[7], "private diagnostic") {
		t.Fatalf("unsafe explanation: %s", api.TrustComments[7])
	}
}

func TestRefusalFinishesAfterRestartWithoutDuplicateComment(t *testing.T) {
	t.Parallel()

	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}, Trust: func(github.Issue) (github.TrustDecision, error) {
		return github.TrustDecision{Reason: "Untrusted label actor."}, nil
	}}
	fail := true
	api.Mutate = func(action, repo string, n int, label string) error {
		if fail && action == "remove" {
			return errors.New("label removal interrupted")
		}
		return nil
	}
	s, rt := testkit.Fixture(t, api)
	if err := s.Tick(context.Background()); err == nil {
		t.Fatal("expected interrupted removal")
	}
	if len(api.TrustComments) != 1 {
		t.Fatal("explanation must precede label removal")
	}
	fail = false
	cfg, _, err := config.Parse([]byte("repositories: [{repo: owner/repo}]"))
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, err := restarted.Runs(context.Background())
	if err != nil || len(runs) != 0 || testkit.Has(api.Issues["owner/repo"][0], "ready-for-agent") || len(api.TrustComments) != 1 {
		t.Fatalf("restart did not finish refusal safely: %+v %v", runs, err)
	}
	queue, err := restarted.Queue(context.Background())
	if err != nil || len(queue) != 1 || queue[0].RefusalReason == "" {
		t.Fatalf("restart lost refusal: %+v %v", queue, err)
	}
}

func TestTrustedRelabelClearsPreviousRefusal(t *testing.T) {
	t.Parallel()

	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}, Trust: func(github.Issue) (github.TrustDecision, error) {
		return github.TrustDecision{Reason: "Untrusted label actor."}, nil
	}, Blockers: map[int][]github.Issue{7: {testkit.Ready(99)}}}
	s, _ := testkit.Fixture(t, api)
	ctx := context.Background()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	api.Trust = func(github.Issue) (github.TrustDecision, error) { return github.TrustDecision{Allowed: true}, nil }
	if err := api.AddLabel(ctx, "owner/repo", 7, "ready-for-agent"); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	queue, err := s.Queue(ctx)
	if err != nil || len(queue) != 1 || queue[0].RefusalReason != "" || queue[0].Attention || len(queue[0].Blockers) != 1 {
		t.Fatalf("trusted relabel did not clear refusal: %+v %v", queue, err)
	}
	if len(api.TrustComments) != 1 {
		t.Fatal("trusted relabel must preserve one historical explanation")
	}
}

func TestTrustedIssueCanBeClaimed(t *testing.T) {
	t.Parallel()

	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}}
	s, _ := testkit.Configured(t, api, "repositories: [{repo: owner/repo}]", testkit.RejectingGit{})
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(context.Background())
	if err != nil || len(runs) != 1 || len(api.TrustComments) != 0 {
		t.Fatalf("trusted issue was not claimed: %+v %v", runs, err)
	}
}

func TestAuthorizationChangedBeforeClaimIsRefused(t *testing.T) {
	t.Parallel()

	checks := 0
	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}, Trust: func(github.Issue) (github.TrustDecision, error) {
		checks++
		if checks == 1 {
			return github.TrustDecision{Allowed: true}, nil
		}
		return github.TrustDecision{Reason: "Label was reapplied by untrusted actor @outsider."}, nil
	}}
	s, _ := testkit.Configured(t, api, "repositories: [{repo: owner/repo}]", testkit.RejectingGit{})
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(context.Background())
	if err != nil || len(runs) != 0 || checks != 2 || len(api.TrustComments) != 1 {
		t.Fatalf("cached approval leaked into claim: checks=%d, runs=%+v, err=%v", checks, runs, err)
	}
}

func TestDashboardRendersRefusedIssueReason(t *testing.T) {
	t.Parallel()

	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}, Trust: func(github.Issue) (github.TrustDecision, error) {
		return github.TrustDecision{Reason: "Untrusted actor <script>alert(1)</script>."}, nil
	}}
	s, rt := testkit.Fixture(t, api)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Parse([]byte("repositories: [{repo: owner/repo}]"))
	if err != nil {
		t.Fatal(err)
	}
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); server.RunUpdates(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		page := httptest.NewRecorder()
		server.ServeHTTP(page, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/queue", nil))
		body := page.Body.String()
		if strings.Contains(body, "Refused:") {
			blocked := strings.Index(body, `id="group-blocked"`)
			refused := strings.Index(body, "Refused:")
			for _, group := range []string{"attention", "merge", "running", "harness", "ready", "blocked"} {
				if !strings.Contains(body, `id="group-`+group+`"`) {
					t.Fatalf("queue lacks group %q: %s", group, body)
				}
			}
			if page.Code != http.StatusOK || !strings.Contains(body, "#7") || blocked < 0 || refused < blocked ||
				!strings.Contains(body, "&lt;script&gt;") || strings.Contains(body, "<script>alert(1)</script>") ||
				strings.Contains(body, ">Repository<") {
				t.Fatalf("unsafe or ungrouped refusal rendering: %d %s", page.Code, body)
			}
			filtered := httptest.NewRecorder()
			server.ServeHTTP(filtered, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/queue?group=blocked", nil))
			narrowed := filtered.Body.String()
			if filtered.Code != http.StatusOK || !strings.Contains(narrowed, `id="group-blocked"`) || strings.Contains(narrowed, `id="group-running"`) {
				t.Fatalf("Queue group filter did not narrow the table: %d %s", filtered.Code, narrowed)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("refusal reason was not rendered")
}

func TestRecoveredClaimWithoutSnapshotCannotExecuteUntrustedBody(t *testing.T) {
	t.Parallel()

	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}, Trust: func(github.Issue) (github.TrustDecision, error) {
		return github.TrustDecision{Reason: "Untrusted edit after authorization."}, nil
	}}
	prepared := false
	s, rt := testkit.Configured(t, api, "repositories: [{repo: owner/repo}]", testkit.RejectingGit{PrepareHook: func(managedgit.PrepareRequest) { prepared = true }})
	ctx := context.Background()
	if _, err := rt.Workflow.Transition(ctx, "interrupted-claim", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7}); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(ctx)
	if err != nil || len(runs) != 1 || runs[0].State != workflow.NeedsAttention || prepared || len(api.TrustComments) != 1 {
		t.Fatalf("recovered claim accepted unchecked fresh content: prepared=%v, runs=%+v, err=%v", prepared, runs, err)
	}
}

func TestPendingRefusalPreservesTrustedRelabelAfterRestart(t *testing.T) {
	t.Parallel()

	for _, removalCompleted := range []bool{false, true} {
		for _, paused := range []bool{false, true} {
			name := fmt.Sprintf("removal_completed=%t/paused=%t", removalCompleted, paused)
			t.Run(name, func(t *testing.T) {
				api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}, Blockers: map[int][]github.Issue{7: {testkit.Ready(99)}}, Trust: func(github.Issue) (github.TrustDecision, error) {
					return github.TrustDecision{Reason: "Untrusted label actor."}, nil
				}}
				s, rt := testkit.Fixture(t, api)
				interrupted, cancel := context.WithCancel(context.Background())
				defer cancel()
				api.Mutate = func(action, repo string, n int, label string) error {
					if action == "remove" && label == "ready-for-agent" {
						if !removalCompleted {
							return errors.New("ready-label removal interrupted")
						}
						// Cancellation after the remote mutation keeps its local receipt pending.
						cancel()
					}
					return nil
				}
				if err := s.Tick(interrupted); err == nil {
					t.Fatal("expected interrupted refusal")
				}
				if len(api.TrustComments) != 1 {
					t.Fatal("initial refusal should have one explanation")
				}
				if testkit.Has(api.Issues["owner/repo"][0], "ready-for-agent") == removalCompleted {
					t.Fatal("interruption did not reach the intended removal boundary")
				}
				ctx := context.Background()
				api.Mutate = nil
				api.Trust = func(github.Issue) (github.TrustDecision, error) { return github.TrustDecision{Allowed: true}, nil }
				if err := api.RemoveLabel(ctx, "owner/repo", 7, "ready-for-agent"); err != nil {
					t.Fatal(err)
				}
				if err := api.AddLabel(ctx, "owner/repo", 7, "ready-for-agent"); err != nil {
					t.Fatal(err)
				}
				cfg, _, err := config.Parse([]byte("repositories: [{repo: owner/repo}]"))
				if err != nil {
					t.Fatal(err)
				}
				restarted, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api})
				if err != nil {
					t.Fatal(err)
				}
				if paused {
					if err := restarted.Pause(ctx, true); err != nil {
						t.Fatal(err)
					}
				}
				if err := restarted.Tick(ctx); err != nil {
					t.Fatal(err)
				}
				queue, err := restarted.Queue(ctx)
				if err != nil || len(queue) != 1 || queue[0].Attention || queue[0].RefusalReason != "" || !testkit.Has(api.Issues["owner/repo"][0], "ready-for-agent") {
					t.Fatalf("pending refusal consumed trusted reauthorization: labels=%+v queue=%+v err=%v", api.Issues["owner/repo"][0].Labels, queue, err)
				}
				runs, err := restarted.Runs(ctx)
				if err != nil || len(runs) != 0 || len(api.TrustComments) != 1 {
					t.Fatalf("replay must preserve queued work and one explanation: runs=%+v err=%v", runs, err)
				}
			})
		}
	}
}

func TestUntrustedTitleEditRefusesClaimWithExplanation(t *testing.T) {
	t.Parallel()

	reason := "Issue title was edited after authorization by untrusted actor @outsider."
	api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}, Trust: func(github.Issue) (github.TrustDecision, error) {
		return github.TrustDecision{Reason: reason}, nil
	}}
	s, _ := testkit.Fixture(t, api)
	ctx := context.Background()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(ctx)
	if err != nil || len(runs) != 0 {
		t.Fatalf("title edit claimed: %+v %v", runs, err)
	}
	if testkit.Has(api.Issues["owner/repo"][0], "ready-for-agent") || testkit.Has(api.Issues["owner/repo"][0], "agent-running") {
		t.Fatal("refusal must remove ready without adding running")
	}
	if api.TrustComments[7] != reason {
		t.Fatalf("title edit explanation: %s", api.TrustComments[7])
	}
}
