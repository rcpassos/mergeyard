package scheduler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type ciGitHub struct {
	*fakeGitHub
	evidence      ci.Evidence
	queryError    error
	readyError    error
	readyCalls    int
	applyReady    bool
	afterReady    func()
	afterEvidence func()
}

func (f *ciGitHub) CheckEvidence(_ context.Context, _, sha, _ string) (ci.Evidence, error) {
	e := f.evidence
	e.SHA = sha
	for i := range e.Checks {
		if e.Checks[i].SHA == "" {
			e.Checks[i].SHA = sha
		}
	}
	if f.afterEvidence != nil {
		f.afterEvidence()
	}
	return e, f.queryError
}
func (f *ciGitHub) MarkReady(_ context.Context, _ string, n int) error {
	f.readyCalls++
	if f.applyReady {
		for _, pr := range f.prs {
			if pr.Number == n {
				pr.Draft = false
			}
		}
	}
	if f.afterReady != nil {
		f.afterReady()
	}
	return f.readyError
}
func TestCINoChecksWindowAndRestartWhilePaused(t *testing.T) {
	_, runtime, base, _, cfg, r := localFlow(t, successfulScript)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	api := &ciGitHub{fakeGitHub: base, applyReady: true}
	deps := scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }}
	s, err := scheduler.New(cfg, schedulerResources(runtime), deps)
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if run.CI == nil || !run.CI.StartedAt.Equal(now) || !run.CI.Deadline.Equal(now.Add(time.Hour)) {
		t.Fatalf("wait not started with approval: %+v", run.CI)
	}
	s.Pause(context.Background(), true)
	now = now.Add(2*time.Minute - time.Nanosecond)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	run, _ = runtime.Workflow.Get(context.Background(), run.ID)
	if run.State != workflow.WaitingForCI || api.readyCalls != 0 {
		t.Fatal("premature readiness")
	}
	// Reopening the scheduler must retain the original wait, even with new config.
	cfg.CITimeout = time.Minute
	s, err = scheduler.New(cfg, schedulerResources(runtime), deps)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Nanosecond)
	s.Pause(context.Background(), true)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	run, _ = runtime.Workflow.Get(context.Background(), run.ID)
	if run.State != workflow.ReadyToMerge || api.readyCalls != 1 || !run.CI.Deadline.Equal(run.CI.StartedAt.Add(time.Hour)) {
		t.Fatalf("readiness: %+v %+v calls %d", run, run.CI, api.readyCalls)
	}
	if api.prs["mergeyard/issue-7"].State != github.Open {
		t.Fatal("merged PR")
	}
}

func TestCICheckMatrix(t *testing.T) {
	for _, tc := range []struct {
		name     string
		checks   []ci.Check
		required []ci.Requirement
		allow    bool
		state    workflow.State
		code     string
	}{
		{name: "required success", checks: []ci.Check{{Name: "build", Source: "check", AppID: 42, Status: "completed", Conclusion: "success"}}, required: []ci.Requirement{{Name: "build", AppID: 42}}, state: workflow.ReadyToMerge},
		{name: "optional pending blocks", checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "success"}, {Name: "optional", Source: "check", Status: "in_progress"}}, state: workflow.WaitingForCI},
		{name: "missing required blocks", required: []ci.Requirement{{Name: "build"}}, state: workflow.WaitingForCI},
		{name: "wrong app blocks", checks: []ci.Check{{Name: "build", Source: "check", AppID: 1, Status: "completed", Conclusion: "success"}}, required: []ci.Requirement{{Name: "build", AppID: 42}}, state: workflow.WaitingForCI},
		{name: "allowed skipped and neutral", allow: true, checks: []ci.Check{{Name: "skip", Source: "check", Status: "completed", Conclusion: "skipped"}, {Name: "neutral", Source: "check", Status: "completed", Conclusion: "neutral"}}, state: workflow.ReadyToMerge},
		{name: "disallowed skipped", checks: []ci.Check{{Name: "skip", Source: "check", Status: "completed", Conclusion: "skipped"}}, state: workflow.WaitingForCI},
		{name: "disallowed neutral", checks: []ci.Check{{Name: "neutral", Source: "check", Status: "completed", Conclusion: "neutral"}}, state: workflow.WaitingForCI},
		{name: "unknown conclusion", checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "future"}}, state: workflow.WaitingForCI},
		{name: "stale", checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "stale"}}, state: workflow.WaitingForCI},
		{name: "wrong check SHA", checks: []ci.Check{{Name: "build", SHA: "old", Source: "check", Status: "completed", Conclusion: "success"}}, state: workflow.WaitingForCI},
		{name: "optional failure", checks: []ci.Check{{Name: "optional", Source: "check", Status: "completed", Conclusion: "failure"}}, state: workflow.NeedsAttention, code: "ci.repair_unavailable"},
		{name: "timed out", checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "timed_out"}}, state: workflow.NeedsAttention, code: "ci.repair_unavailable"},
		{name: "cancelled", checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "cancelled"}}, state: workflow.NeedsAttention, code: "ci.action_required"},
		{name: "action required", checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "action_required"}}, state: workflow.NeedsAttention, code: "ci.action_required"},
		{name: "legacy failure", checks: []ci.Check{{Name: "legacy", Source: "status", Conclusion: "failure"}}, state: workflow.NeedsAttention, code: "ci.repair_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, runtime, base, _, cfg, r := localFlow(t, successfulScript)
			now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
			api := &ciGitHub{fakeGitHub: base, evidence: ci.Evidence{Checks: tc.checks, Required: tc.required, AllowSkippedNeutral: tc.allow}, applyReady: true}
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			run := finish(t, s, workflow.WaitingForCI, workflow.Review)
			now = now.Add(3 * time.Minute)
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			got, err := runtime.Workflow.Get(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != tc.state || got.LastErrorCode != tc.code || (tc.state != workflow.ReadyToMerge && api.readyCalls != 0) {
				t.Fatalf("state=%s error=%s writes=%d", got.State, got.LastErrorCode, api.readyCalls)
			}
			if got.CI == nil || len(got.CI.Evidence.Checks) != len(tc.checks) {
				t.Fatal("check evidence not retained")
			}
			var attempts int
			if err := runtime.DB.QueryRow("SELECT count(*) FROM phase_attempts WHERE run_id=?", run.ID).Scan(&attempts); err != nil || attempts != 2 {
				t.Fatalf("CI launched an agent: %d %v", attempts, err)
			}
		})
	}
}

func TestCITimeoutRetainsDeadlineAcrossUnknownAndRestart(t *testing.T) {
	_, runtime, base, _, cfg, r := localFlow(t, successfulScript)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	api := &ciGitHub{fakeGitHub: base, evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "queued"}}}}
	deps := scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }}
	s, err := scheduler.New(cfg, schedulerResources(runtime), deps)
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
	now = now.Add(30 * time.Minute)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	api.queryError = fmt.Errorf("requirements permission denied")
	s, err = scheduler.New(cfg, schedulerResources(runtime), deps)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(30*time.Minute - time.Nanosecond)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, _ := runtime.Workflow.Get(context.Background(), run.ID)
	if saved.State != workflow.WaitingForCI || saved.CI.QueryError == "" || !saved.CI.Deadline.Equal(run.CI.Deadline) {
		t.Fatalf("renewed or lost wait: %+v", saved.CI)
	}
	now = now.Add(time.Nanosecond)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, _ = runtime.Workflow.Get(context.Background(), run.ID)
	if saved.State != workflow.NeedsAttention || saved.LastErrorCode != "ci.wait_timeout" || api.readyCalls != 0 {
		t.Fatalf("timeout: %+v", saved)
	}
}

func TestCIHeadRacesAndReadinessAmbiguity(t *testing.T) {
	for _, mode := range []string{"before checks", "during checks", "after mutation", "write applied with error", "write not applied", "already normal", "commit acknowledgement failed", "query crosses deadline"} {
		t.Run(mode, func(t *testing.T) {
			_, runtime, base, _, cfg, r := localFlow(t, successfulScript)
			now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
			api := &ciGitHub{fakeGitHub: base, evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "success"}}}, applyReady: true}
			deps := scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }}
			s, err := scheduler.New(cfg, schedulerResources(runtime), deps)
			if err != nil {
				t.Fatal(err)
			}
			run := finish(t, s, workflow.WaitingForCI, workflow.Review)
			pr := base.prs["mergeyard/issue-7"]
			switch mode {
			case "before checks":
				pr.Head.SHA = "replacement"
			case "during checks":
				api.afterEvidence = func() { pr.Head.SHA = "replacement" }
			case "after mutation":
				api.afterReady = func() { pr.Head.SHA = "replacement" }
			case "write applied with error":
				api.readyError = fmt.Errorf("connection lost")
			case "write not applied":
				api.readyError = fmt.Errorf("permission denied")
				api.applyReady = false
			case "already normal":
				pr.Draft = false
			case "query crosses deadline":
				api.afterEvidence = func() { now = now.Add(time.Hour) }
			case "commit acknowledgement failed":
				if _, err := runtime.DB.Exec(`CREATE TRIGGER reject_ready BEFORE INSERT ON events WHEN NEW.type='ci.updated' AND NEW.payload_json LIKE '%ci_passed%' BEGIN SELECT RAISE(FAIL,'crash'); END`); err != nil {
					t.Fatal(err)
				}
			}
			err = s.Tick(context.Background())
			if mode == "commit acknowledgement failed" {
				if err == nil {
					t.Fatal("ready acknowledgement should fail")
				}
				saved, _ := runtime.Workflow.Get(context.Background(), run.ID)
				if saved.State != workflow.WaitingForCI || !saved.CI.ReadyStarted {
					t.Fatal("readiness intent lost")
				}
				runtime.DB.Exec("DROP TRIGGER reject_ready")
				s, err = scheduler.New(cfg, schedulerResources(runtime), deps)
				if err != nil {
					t.Fatal(err)
				}
				err = s.Tick(context.Background())
			}
			if err != nil {
				t.Fatal(err)
			}
			saved, _ := runtime.Workflow.Get(context.Background(), run.ID)
			switch mode {
			case "before checks", "during checks", "after mutation":
				if saved.State != workflow.NeedsAttention || saved.ApprovedSHA != "" || saved.LastErrorCode != "ci.head_changed" {
					t.Fatalf("stale readiness: %+v", saved)
				}
			case "query crosses deadline":
				if saved.State != workflow.NeedsAttention || saved.LastErrorCode != "ci.wait_timeout" || api.readyCalls != 0 {
					t.Fatalf("deadline crossed: %+v", saved)
				}
			case "write not applied":
				if saved.State != workflow.NeedsAttention || !saved.CI.ReadyStarted {
					t.Fatalf("ambiguity lost: %+v", saved)
				}
			default:
				if saved.State != workflow.ReadyToMerge {
					t.Fatalf("readiness: %+v", saved)
				}
			}
			calls := api.readyCalls
			for range 2 {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if api.readyCalls != calls || (mode == "already normal" && calls != 0) {
				t.Fatal("readiness write replayed")
			}
		})
	}
}

func TestReadyWarningsClosureCapacityAndHTTP(t *testing.T) {
	_, runtime, base, _, cfg, r := localFlow(t, successfulScript)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	api := &ciGitHub{fakeGitHub: base, evidence: ci.Evidence{AllowSkippedNeutral: true, Checks: []ci.Check{{Name: "<script>check</script>", Source: "check", Status: "completed", Conclusion: "neutral", URL: "javascript:alert(1)"}}}, applyReady: true}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, _ := runtime.Workflow.Get(context.Background(), run.ID)
	if saved.State != workflow.ReadyToMerge {
		t.Fatal("not ready")
	}
	approved := saved.ApprovedSHA
	api.prs["mergeyard/issue-7"].Head.SHA = "later-commit"
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, _ = runtime.Workflow.Get(context.Background(), run.ID)
	if saved.State != workflow.ReadyToMerge || saved.ApprovedSHA != approved || !strings.Contains(saved.CI.Warning, "changed-after-approval") {
		t.Fatalf("post-readiness warning: %+v", saved)
	}
	server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/runs/" + run.ID, "/"} {
		res := httptest.NewRecorder()
		server.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331"+path, nil))
		body := res.Body.String()
		if res.Code != 200 || !strings.Contains(body, "Waiting for your merge") {
			t.Fatalf("HTTP %d: %s", res.Code, body)
		}
		if path != "/" {
			for _, text := range []string{"Wait started", "Wait deadline", "neutral", "passed", "&lt;script&gt;check&lt;/script&gt;", "later-commit", "changed-after-approval"} {
				if !strings.Contains(body, text) {
					t.Fatalf("missing %q", text)
				}
			}
			if strings.Contains(body, `href="javascript:alert`) || strings.Contains(body, "<script>check") {
				t.Fatal("unescaped check content")
			}
		} else if !strings.Contains(body, "0 / 1") {
			t.Fatal("dashboard still consumes slot")
		}
	}
	res := httptest.NewRecorder()
	server.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/api/status", nil))
	var status web.Status
	if err := json.Unmarshal(res.Body.Bytes(), &status); err != nil || len(status.Runs) != 1 || status.Runs[0].CI.Warning == "" {
		t.Fatal("API lost CI evidence")
	}
	res = httptest.NewRecorder()
	server.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/static/app.js", nil))
	if !strings.Contains(res.Body.String(), "ci.updated") || !strings.Contains(res.Body.String(), "pr.readiness_started") {
		t.Fatal("live page lacks CI events")
	}
	// A ready PR frees the one slot for unrelated work.
	api.issues["owner/repo"] = append(api.issues["owner/repo"], ready(8))
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(context.Background())
	if err != nil || len(runs) != 2 {
		t.Fatalf("capacity not released: %+v %v", runs, err)
	}
	api.prs["mergeyard/issue-7"].State = github.Closed
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, _ = runtime.Workflow.Get(context.Background(), run.ID)
	if saved.State != workflow.NeedsAttention || saved.LastErrorCode != "ci.pr_closed" {
		t.Fatalf("closure: %+v", saved)
	}
}

// Resolve the PR head from the managed branch as implementation/fixes publish.
type ciBranchGitHub struct{ *ciGitHub }

func (f ciBranchGitHub) GetPullRequest(ctx context.Context, repo string, n int) (*github.PullRequest, error) {
	pr, err := f.fakeGitHub.GetPullRequest(ctx, repo, n)
	if err == nil && pr != nil {
		pr.Head.SHA = f.head(pr.Head.Ref)
	}
	return pr, err
}

func TestCIReadinessAfterMergedImplementerFlows(t *testing.T) {
	for _, mode := range []string{"Claude fix and re-review", "Codex implementation"} {
		t.Run(mode, func(t *testing.T) {
			setup := localFlow
			script := loopScript(`printf fixed > feature.txt; ` + fixedReport)
			if mode == "Codex implementation" {
				setup = codexFlow
				script = codexImplementation
			}
			_, runtime, base, _, cfg, r := setup(t, script)
			now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
			api := &ciGitHub{fakeGitHub: base, applyReady: true, evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "success"}}}}
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: ciBranchGitHub{api}, Runner: r, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			run := finish(t, s, workflow.WaitingForCI, workflow.Review)
			if run.CI == nil || run.CI.SHA != run.ApprovedSHA || !run.CI.StartedAt.Equal(now) {
				t.Fatal("approval did not start a pinned CI wait")
			}
			if mode == "Claude fix and re-review" {
				if run.ReviewRound != 2 || run.Fix == nil || !run.Fix.Pushed {
					t.Fatal("fix history lost")
				}
			} else if run.Implementer == nil || run.Implementer.Agent != "codex" || run.Implementer.SessionID != codexID {
				t.Fatal("Codex session lost")
			}
			if _, err := s.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			saved, err := runtime.Workflow.Get(context.Background(), run.ID)
			if err != nil || saved.State != workflow.ReadyToMerge || api.readyCalls != 1 || saved.CI.CurrentHead != saved.ApprovedSHA {
				t.Fatalf("merged flow readiness: %+v %v", saved, err)
			}
		})
	}
}
