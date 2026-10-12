package scheduler_test

import (
	"context"
	"errors"
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
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestUntrustedIssueNeverClaimedAndRefusalVisible(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}, trust: func(github.Issue) (github.TrustDecision, error) {
		return github.TrustDecision{Reason: "Label was applied by untrusted actor @outsider."}, nil
	}}
	s, _ := fixture(t, api)
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
	if has(api.issues["owner/repo"][0], "ready-for-agent") || has(api.issues["owner/repo"][0], "agent-running") {
		t.Fatal("refusal must remove ready without adding running")
	}
	if len(api.trustComments) != 1 || !strings.Contains(api.trustComments[7], "@outsider") {
		t.Fatalf("refusal comments: %+v", api.trustComments)
	}
	queue, err := s.Queue(ctx)
	if err != nil || len(queue) != 1 || !queue[0].Attention || !strings.Contains(queue[0].RefusalReason, "@outsider") {
		t.Fatalf("refusal absent from dashboard: %+v %v", queue, err)
	}
}

func TestUnreadableAuthorizationFailsClosed(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}, trust: func(github.Issue) (github.TrustDecision, error) {
		return github.TrustDecision{}, &fault.Error{Code: "github.forbidden", Message: "private diagnostic"}
	}}
	s, _ := fixture(t, api)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(context.Background())
	if err != nil || len(runs) != 0 {
		t.Fatalf("unverified issue claimed: %+v %v", runs, err)
	}
	if !strings.Contains(api.trustComments[7], "github.forbidden") || strings.Contains(api.trustComments[7], "private diagnostic") {
		t.Fatalf("unsafe explanation: %s", api.trustComments[7])
	}
}

func TestRefusalFinishesAfterRestartWithoutDuplicateComment(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}, trust: func(github.Issue) (github.TrustDecision, error) {
		return github.TrustDecision{Reason: "Untrusted label actor."}, nil
	}}
	fail := true
	api.mutate = func(action, repo string, n int, label string) error {
		if fail && action == "remove" {
			return errors.New("label removal interrupted")
		}
		return nil
	}
	s, rt := fixture(t, api)
	if err := s.Tick(context.Background()); err == nil {
		t.Fatal("expected interrupted removal")
	}
	if len(api.trustComments) != 1 {
		t.Fatal("explanation must precede label removal")
	}
	fail = false
	cfg, _, err := config.Parse([]byte("repositories: [{repo: owner/repo}]"))
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, err := restarted.Runs(context.Background())
	if err != nil || len(runs) != 0 || has(api.issues["owner/repo"][0], "ready-for-agent") || len(api.trustComments) != 1 {
		t.Fatalf("restart did not finish refusal safely: %+v %v", runs, err)
	}
	queue, err := restarted.Queue(context.Background())
	if err != nil || len(queue) != 1 || queue[0].RefusalReason == "" {
		t.Fatalf("restart lost refusal: %+v %v", queue, err)
	}
}

func TestTrustedRelabelClearsPreviousRefusal(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}, trust: func(github.Issue) (github.TrustDecision, error) {
		return github.TrustDecision{Reason: "Untrusted label actor."}, nil
	}, blockers: map[int][]github.Issue{7: {ready(99)}}}
	s, _ := fixture(t, api)
	ctx := context.Background()
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	api.trust = func(github.Issue) (github.TrustDecision, error) { return github.TrustDecision{Allowed: true}, nil }
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
	if len(api.trustComments) != 1 {
		t.Fatal("trusted relabel must preserve one historical explanation")
	}
}

func TestTrustedIssueCanBeClaimed(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}}
	s, _ := configured(t, api, "repositories: [{repo: owner/repo}]", rejectingGit{})
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(context.Background())
	if err != nil || len(runs) != 1 || len(api.trustComments) != 0 {
		t.Fatalf("trusted issue was not claimed: %+v %v", runs, err)
	}
}

func TestAuthorizationChangedBeforeClaimIsRefused(t *testing.T) {
	checks := 0
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}, trust: func(github.Issue) (github.TrustDecision, error) {
		checks++
		if checks == 1 {
			return github.TrustDecision{Allowed: true}, nil
		}
		return github.TrustDecision{Reason: "Label was reapplied by untrusted actor @outsider."}, nil
	}}
	s, _ := configured(t, api, "repositories: [{repo: owner/repo}]", rejectingGit{})
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(context.Background())
	if err != nil || len(runs) != 0 || checks != 2 || len(api.trustComments) != 1 {
		t.Fatalf("cached approval leaked into claim: checks=%d, runs=%+v, err=%v", checks, runs, err)
	}
}

func TestDashboardRendersRefusedIssueReason(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}, trust: func(github.Issue) (github.TrustDecision, error) {
		return github.TrustDecision{Reason: "Untrusted actor <script>alert(1)</script>."}, nil
	}}
	s, rt := fixture(t, api)
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
		server.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/queue", nil))
		body := page.Body.String()
		if strings.Contains(body, "Refused:") {
			if page.Code != http.StatusOK || !strings.Contains(body, "owner/repo #7") || !strings.Contains(body, "&lt;script&gt;") || strings.Contains(body, "<script>alert(1)</script>") {
				t.Fatalf("unsafe refusal rendering: %d %s", page.Code, body)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("refusal reason was not rendered")
}

func TestRecoveredClaimWithoutSnapshotCannotExecuteUntrustedBody(t *testing.T) {
	api := &fakeGitHub{issues: map[string][]github.Issue{"owner/repo": {ready(7)}}, trust: func(github.Issue) (github.TrustDecision, error) {
		return github.TrustDecision{Reason: "Untrusted edit after authorization."}, nil
	}}
	prepared := false
	s, rt := configured(t, api, "repositories: [{repo: owner/repo}]", rejectingGit{prepare: func(managedgit.PrepareRequest) { prepared = true }})
	ctx := context.Background()
	if _, err := rt.Workflow.Transition(ctx, "interrupted-claim", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7}); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(ctx)
	if err != nil || len(runs) != 1 || runs[0].State != workflow.NeedsAttention || prepared || len(api.trustComments) != 1 {
		t.Fatalf("recovered claim accepted unchecked fresh content: prepared=%v, runs=%+v, err=%v", prepared, runs, err)
	}
}

func TestAllPhasesUseAuthorizedIssueSnapshot(t *testing.T) {
	checkInput := `phase_dir=''
previous=''
for arg do
 if [ "$previous" = '--add-dir' ]; then phase_dir=$arg; fi
 previous=$arg
 done
if ! /usr/bin/grep -Fq 'APPROVED-ISSUE-SNAPSHOT' "$phase_dir/input.md" ||
   /usr/bin/grep -Fq 'UNTRUSTED-BODY-EDIT' "$phase_dir/input.md"; then
 printf 'unauthorized issue snapshot reached harness\n' >&2
 exit 9
fi
`
	_, rt, api, _, cfg, r := localFlow(t, checkInput+loopScript("printf repaired > feature.txt\n"+fixedReport))
	api.issues["owner/repo"][0].Body = "APPROVED-ISSUE-SNAPSHOT"
	reads := 0
	api.getIssue = func(repo string, n int) (github.Issue, error) {
		reads++
		issue := api.issues[repo][0]
		if reads > 1 {
			issue.Body = "UNTRUSTED-BODY-EDIT"
		}
		return issue, nil
	}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	var run workflow.Run
	waitForWithin(t, 45*time.Second, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != 1 {
			return false
		}
		run = runs[0]
		return run.State == workflow.WaitingForCI || run.State == workflow.NeedsAttention
	})
	if run.State != workflow.WaitingForCI || run.ReviewRound != 2 {
		t.Fatalf("unchecked refreshed body reached implement/review/fix: %+v", run)
	}
}
