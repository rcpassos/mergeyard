package testkit

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
)

func (f *FakeGitHub) UpdatePullRequest(ctx context.Context, repo string, n int, content github.PullRequestContent) (*github.PullRequest, error) {
	pr, err := f.GetPullRequest(ctx, repo, n)
	if err != nil {
		return nil, err
	}
	body, err := github.UpdatePullRequestBody(pr.Body, content)
	if err != nil {
		return nil, err
	}
	pr.Body = body
	return pr, nil
}

type EvidenceGitHub struct {
	BranchGitHub
	Offline bool
	Code    string
	Updates int
}

func (f *EvidenceGitHub) UpdatePullRequest(ctx context.Context, repo string, n int, content github.PullRequestContent) (*github.PullRequest, error) {
	f.Updates++
	if f.Offline {
		code := f.Code
		if code == "" {
			code = "github.unavailable"
		}
		return nil, &fault.Error{Code: code, Message: "Offline"}
	}
	return f.FakeGitHub.UpdatePullRequest(ctx, repo, n, content)
}

type StalledEvidenceGitHub struct {
	BranchGitHub
	Bounded bool
}

func (f *StalledEvidenceGitHub) UpdatePullRequest(ctx context.Context, _ string, _ int, _ github.PullRequestContent) (*github.PullRequest, error) {
	deadline, ok := ctx.Deadline()
	f.Bounded = ok && time.Until(deadline) <= 5100*time.Millisecond
	if !f.Bounded {
		return nil, context.DeadlineExceeded
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// seedVerificationRun inserts a run and its latest implement evidence without
// launching an agent, so publication reconciliation owns it immediately.
func SeedVerificationRun(t *testing.T, rt *app.Runtime, id, state string, issue, pr int) {
	t.Helper()
	ctx := context.Background()
	if _, err := rt.DB.ExecContext(ctx, `INSERT INTO runs(id,repository,issue_number,state,pr_number) VALUES(?,'owner/repo',?,?,?)`, id, issue, state, pr); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.DB.ExecContext(ctx, `INSERT INTO scheduler_runs(run_id,issue_json) VALUES(?,?)`, id, fmt.Sprintf(`{"number":%d,"title":"Feature"}`, issue)); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.DB.ExecContext(ctx, `INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status,step,report_json,report_eligible) VALUES(?,?,'implement','implementer',0,1,'claude','succeeded','',?,1)`, id+"-attempt", id,
		`{"schema_version":4,"criteria":[],"human_action":null,"status":"success","summary":"Valid evidence","verification":[{"command":"test","outcome":"passed","excerpt":null}]}`); err != nil {
		t.Fatal(err)
	}
}

func CountEvents(t *testing.T, rt *app.Runtime, query string, args ...any) int {
	t.Helper()
	var count int
	if err := rt.DB.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// failingVerificationFlow builds a runtime whose PR body updates fail with code.
func FailingVerificationFlow(t *testing.T, code string) (*scheduler.Scheduler, *app.Runtime, *EvidenceGitHub, config.Config) {
	t.Helper()
	api := &FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {Ready(7)}}, Prs: map[string]*github.PullRequest{
		"feature": {Number: 101, State: github.Open, Body: "human body"},
	}, Head: func(string) string { return "" }}
	_, rt := Fixture(t, api)
	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
	if err != nil {
		t.Fatal(err)
	}
	publisher := &EvidenceGitHub{BranchGitHub: BranchGitHub{api}, Offline: true, Code: code}
	s, err := TestScheduler(cfg, SchedulerResources(rt), scheduler.Dependencies{GitHub: publisher})
	if err != nil {
		t.Fatal(err)
	}
	return s, rt, publisher, cfg
}

func FailingVerificationFlowAt(t *testing.T, code string, now func() time.Time) (*scheduler.Scheduler, *app.Runtime, *EvidenceGitHub, config.Config) {
	t.Helper()
	_, rt, publisher, cfg := FailingVerificationFlow(t, code)
	s, err := TestScheduler(cfg, SchedulerResources(rt), scheduler.Dependencies{GitHub: publisher, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return s, rt, publisher, cfg
}

type FiniteSlowBodyGitHub struct {
	*FakeGitHub
	Calls []int
}

func (f *FiniteSlowBodyGitHub) UpdatePullRequest(ctx context.Context, repo string, n int, content github.PullRequestContent) (*github.PullRequest, error) {
	f.Calls = append(f.Calls, n)
	if n == 101 {
		timer := time.NewTimer(6 * time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil, &fault.Error{Code: "github.unavailable", Message: "Finite slow failure for only first PR"}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.FakeGitHub.UpdatePullRequest(ctx, repo, n, content)
}
