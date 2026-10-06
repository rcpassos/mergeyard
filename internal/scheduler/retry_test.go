package scheduler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/web"
	"html"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestExplicitRetryDefaultAttemptKeepsRunAndHistory(t *testing.T) {
	s, runtime, _, _, _, _ := localFlow(t, `if [ ! -f retry-marker ]; then printf preserved > retry-marker; exit 1; fi
`+successfulScript)
	ctx := context.Background()
	before := finish(t, s, workflow.NeedsAttention, workflow.Implement)
	if before.Implementer.Attempt != 1 {
		t.Fatal("fixture did not exhaust default attempt")
	}
	selected, err := s.Retry(ctx, before.ID)
	if err != nil || selected.State != workflow.Active || selected.Phase != workflow.Implement {
		t.Fatalf("retry: %+v %v", selected, err)
	}
	if _, err := s.Retry(ctx, before.ID); err != nil {
		t.Fatal(err)
	}
	after := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if after.ID != before.ID || after.Implementer.Attempt != 2 || after.ReviewRound != 1 {
		t.Fatalf("retry lost identity/budget: %+v", after)
	}
	var attempts int
	if err := runtime.DB.QueryRow("SELECT count(*) FROM phase_attempts WHERE run_id=? AND phase='implement'", after.ID).Scan(&attempts); err != nil || attempts != 2 {
		t.Fatalf("attempts: %d %v", attempts, err)
	}
}

func TestExplicitRetryRenewsCIWithoutAgentOrRound(t *testing.T) {
	_, runtime, base, _, cfg, r := localFlow(t, successfulScript)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	api := &ciGitHub{fakeGitHub: base, applyReady: true, evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "queued"}}}}
	deps := scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }}
	s, err := scheduler.New(cfg, schedulerResources(runtime), deps)
	if err != nil {
		t.Fatal(err)
	}
	before := finish(t, s, workflow.WaitingForCI, workflow.Review)
	now = before.CI.Deadline
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	selected, err := s.Retry(context.Background(), before.ID)
	if err != nil || selected.State != workflow.WaitingForCI || selected.ApprovedSHA != before.ApprovedSHA || !selected.CI.Deadline.Equal(now.Add(cfg.CITimeout)) {
		t.Fatalf("renewal: %+v %v", selected, err)
	}
	if has(base.issues["owner/repo"][0], "agent-needs-attention") || !has(base.issues["owner/repo"][0], "agent-running") {
		t.Fatal("CI retry did not reconcile progress labels")
	}
	deadline := selected.CI.Deadline
	if _, err := s.Retry(context.Background(), before.ID); err != nil {
		t.Fatal(err)
	}
	s, err = scheduler.New(cfg, schedulerResources(runtime), deps)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := runtime.Workflow.Get(context.Background(), before.ID)
	if err != nil || !after.CI.Deadline.Equal(deadline) || after.ReviewRound != before.ReviewRound {
		t.Fatalf("restart: %+v %v", after, err)
	}
	var attempts int
	if err := runtime.DB.QueryRow("SELECT count(*) FROM phase_attempts WHERE run_id=?", after.ID).Scan(&attempts); err != nil || attempts != 2 {
		t.Fatalf("CI spent attempt: %d %v", attempts, err)
	}
}

// The same fixtures cover every supported role pairing through explicit round
// recovery, CI timeout recovery, readiness, observed merge, and safe maintenance.
func TestExplicitRetryM2Pairings(t *testing.T) {
	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			t.Run(implementer+"/"+reviewer, func(t *testing.T) {
				_, runtime, base, _, cfg, r := pairingFlow(t, implementer, reviewer, `printf fixed > feature.txt; `+fixedReport)
				cfg.MaxRounds = 1
				now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
				api := &retryMergeGitHub{mergeGitHub: &mergeGitHub{ciGitHub: &ciGitHub{fakeGitHub: base, applyReady: true, evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "queued"}}}}, applyClose: true}}
				deps := scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }}
				s, err := scheduler.New(cfg, schedulerResources(runtime), deps)
				if err != nil {
					t.Fatal(err)
				}
				before := finish(t, s, workflow.NeedsAttention, workflow.Review)
				if before.LastErrorCode != "review.max_rounds_exceeded" {
					t.Fatalf("unexpected exhaustion: %+v", before)
				}
				selected, err := s.Retry(context.Background(), before.ID)
				if err != nil || selected.Phase != workflow.Fix || len(selected.Retries) != 1 || selected.Retries[0].GrantedRound != 2 || selected.ReviewRound != 1 {
					t.Fatalf("grant: %+v %v", selected, err)
				}
				if _, err := s.Retry(context.Background(), before.ID); err != nil {
					t.Fatal(err)
				}
				// Restart after selection, before any fix execution.
				s, err = scheduler.New(cfg, schedulerResources(runtime), deps)
				if err != nil {
					t.Fatal(err)
				}
				approved := finish(t, s, workflow.WaitingForCI, workflow.Review)
				if approved.ReviewRound != 2 || len(approved.Retries) != 1 || len(approved.FixHistory) != 1 || len(approved.ReviewHistory) != 2 {
					t.Fatalf("duplicate/lost grant: %+v", approved)
				}
				now = approved.CI.Deadline
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				renewed, err := s.Retry(context.Background(), before.ID)
				if err != nil || renewed.State != workflow.WaitingForCI || renewed.ReviewRound != 2 || renewed.Retries[1].GrantedRound != 0 {
					t.Fatalf("CI retry: %+v %v", renewed, err)
				}
				api.evidence.Checks[0].Status = "completed"
				api.evidence.Checks[0].Conclusion = "success"
				ready := finish(t, s, workflow.ReadyToMerge, workflow.Review)
				api.prs["mergeyard/issue-7"].Merged = true
				api.prs["mergeyard/issue-7"].State = github.Closed
				completed := finish(t, s, workflow.Completed, workflow.Review)
				if completed.ID != before.ID || completed.Merge == nil || completed.Merge.Pending() || api.creations != 1 || api.readyCalls != 1 || ready.ApprovedSHA != approved.ApprovedSHA {
					t.Fatalf("completion: %+v", completed)
				}
				var attempts int
				if err := runtime.DB.QueryRow("SELECT count(*) FROM phase_attempts WHERE run_id=?", completed.ID).Scan(&attempts); err != nil || attempts != 4 {
					t.Fatalf("attempts=%d %v", attempts, err)
				}
			})
		}
	}
}

type retryMergeGitHub struct{ *mergeGitHub }

func (f *retryMergeGitHub) GetPullRequest(ctx context.Context, repo string, n int) (*github.PullRequest, error) {
	pr, err := f.fakeGitHub.GetPullRequest(ctx, repo, n)
	if err == nil && pr != nil {
		pr.Head.SHA = f.head(pr.Head.Ref)
	}
	return pr, err
}

func TestExplicitRetryChangedHeadAndPreservedEdits(t *testing.T) {
	for _, mode := range []string{"published edit", "dirty edit", "divergent commit", "closed PR", "merged PR", "failed merged PR"} {
		t.Run(mode, func(t *testing.T) {
			_, runtime, base, _, cfg, r := localFlow(t, successfulScript)
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			api := &retryMergeGitHub{mergeGitHub: &mergeGitHub{ciGitHub: &ciGitHub{fakeGitHub: base, applyReady: true, evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "queued"}}}}, applyClose: true}}
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			before := finish(t, s, workflow.WaitingForCI, workflow.Review)
			now = before.CI.Deadline
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(runtime.Workspace.Root, "worktrees", "owner-repo", before.ID)
			preserved := []byte("human work\n")
			expectedCode := ""
			switch mode {
			case "published edit", "divergent commit":
				if err := os.WriteFile(filepath.Join(path, "feature.txt"), preserved, 0600); err != nil {
					t.Fatal(err)
				}
				gitCommand(t, path, "add", "feature.txt")
				gitCommand(t, path, "commit", "-m", "human edit")
				if mode == "published edit" {
					gitCommand(t, path, "push", "origin", "HEAD")
				} else {
					expectedCode = "retry.head_diverged"
				}
			case "dirty edit":
				if err := os.WriteFile(filepath.Join(path, "feature.txt"), preserved, 0600); err != nil {
					t.Fatal(err)
				}
				expectedCode = "retry.worktree_dirty"
			case "closed PR":
				api.prs["mergeyard/issue-7"].State = github.Closed
				expectedCode = "retry.pr_closed"
			case "merged PR", "failed merged PR":
				if mode == "failed merged PR" {
					if _, err := runtime.Workflow.Transition(context.Background(), before.ID, workflow.Request{Trigger: workflow.InternalFailure, Failure: &fault.Error{Code: "internal.test", Message: "runtime failed"}}); err != nil {
						t.Fatal(err)
					}
				}
				api.prs["mergeyard/issue-7"].State = github.Closed
				api.prs["mergeyard/issue-7"].Merged = true
			}
			selected, err := s.Retry(context.Background(), before.ID)
			if expectedCode != "" {
				var coded *fault.Error
				if !errors.As(err, &coded) || coded.Code != expectedCode || selected.State != workflow.NeedsAttention {
					t.Fatalf("unsafe retry: %+v %v", selected, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if mode == "published edit" {
				if selected.State != workflow.Active || selected.Phase != workflow.Review || selected.ApprovedSHA != "" || selected.ReviewRound != 1 {
					t.Fatalf("stale approval: %+v", selected)
				}
				after := finish(t, s, workflow.WaitingForCI, workflow.Review)
				if after.Review.Attempt != 2 || after.Review.TargetSHA == before.ApprovedSHA || after.ReviewRound != 1 {
					t.Fatalf("changed head not reviewed: %+v", after)
				}
			}
			if strings.Contains(mode, "edit") || mode == "divergent commit" {
				data, err := os.ReadFile(filepath.Join(path, "feature.txt"))
				if err != nil || !bytes.Equal(data, preserved) {
					t.Fatalf("human work lost: %q %v", data, err)
				}
			}
			if strings.Contains(mode, "merged PR") && mode != "closed PR" {
				if selected.State != workflow.Completed || selected.Merge == nil || selected.Merge.Pending() {
					t.Fatalf("merged retry: %+v", selected)
				}
			}
			if mode != "published edit" {
				var attempts int
				if err := runtime.DB.QueryRow("SELECT count(*) FROM phase_attempts WHERE run_id=?", before.ID).Scan(&attempts); err != nil || attempts != 2 {
					t.Fatalf("unsafe retry spent attempt: %d %v", attempts, err)
				}
			}
		})
	}
}

func TestExplicitRetryDoesNotReplenishMissingSessionRecovery(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		for _, phase := range []workflow.Phase{workflow.Implement, workflow.Review, workflow.Fix} {
			t.Run(agent+"/"+string(phase), func(t *testing.T) {
				s, runtime, api, _, cfg, r := recoveryFlow(t, agent, phase, "exhausted")
				before := finish(t, s, workflow.NeedsAttention, phase)
				if before.LastErrorCode != "harness.session_resume_failed" || len(before.SessionRecoveries) != 1 {
					t.Fatalf("fixture: %+v", before)
				}
				root := runtime.Workspace.Root
				if err := runtime.Close(); err != nil {
					t.Fatal(err)
				}
				restarted, err := app.Open(context.Background(), root)
				if err != nil {
					t.Fatal(err)
				}
				defer restarted.Close()
				s, err = scheduler.New(cfg, schedulerResources(restarted), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
				if err != nil {
					t.Fatal(err)
				}
				for range 2 {
					got, err := s.Retry(context.Background(), before.ID)
					var coded *fault.Error
					if !errors.As(err, &coded) || coded.Code != "harness.session_resume_failed" || got.State != workflow.NeedsAttention || len(got.SessionRecoveries) != 1 {
						t.Fatalf("replenished recovery: %+v %v", got, err)
					}
				}
				var count int
				if err := restarted.DB.QueryRow("SELECT count(*) FROM phase_attempts WHERE run_id=? AND phase=?", before.ID, phase).Scan(&count); err != nil || count != map[workflow.Phase]int{workflow.Implement: 3, workflow.Fix: 3, workflow.Review: 4}[phase] {
					t.Fatalf("recovery spent extra attempt: %d %v", count, err)
				}
			})
		}
	}
}

func TestExplicitRetryIntentAndGrantSurviveRestart(t *testing.T) {
	for _, point := range []string{"before-selection", "after-selection"} {
		t.Run(point, func(t *testing.T) {
			_, runtime, api, _, cfg, r := localFlow(t, loopScript(`printf fixed > feature.txt; `+fixedReport))
			cfg.MaxRounds = 1
			git := &retryCancelGit{Manager: managedgit.New(runtime.Workspace)}
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Git: git, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			before := finish(t, s, workflow.NeedsAttention, workflow.Review)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if point == "before-selection" {
				git.cancel = cancel
			} else {
				api.mutate = func(action, repo string, n int, label string) error {
					if action == "add" && label == "agent-running" {
						return errors.New("labels unavailable")
					}
					return nil
				}
			}
			selected, err := s.Retry(ctx, before.ID)
			selected, _ = runtime.Workflow.Get(context.Background(), before.ID)
			if err == nil {
				t.Fatal("fixture did not interrupt retry")
			}
			if len(selected.Retries) != 1 || (point == "before-selection" && !selected.Retries[0].Pending) || (point == "after-selection" && (selected.Retries[0].Pending || selected.Retries[0].GrantedRound != 2)) {
				t.Fatalf("lost durable boundary: %+v", selected)
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
			api.mutate = nil
			s, err = scheduler.New(cfg, schedulerResources(reopened), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			approved := finish(t, s, workflow.WaitingForCI, workflow.Review)
			if approved.ID != before.ID || approved.ReviewRound != 2 || len(approved.Retries) != 1 || approved.Retries[0].GrantedRound != 2 || approved.Retries[0].Pending || len(approved.FixHistory) != 1 {
				t.Fatalf("restart duplicated intent/grant: %+v", approved)
			}
		})
	}
}

type retryCancelGit struct {
	*managedgit.Manager
	cancel context.CancelFunc
}

func (g *retryCancelGit) InspectRetry(ctx context.Context, run managedgit.Run, target string, edits bool) (string, error) {
	if g.cancel != nil {
		g.cancel()
		return "", ctx.Err()
	}
	return g.Manager.InspectRetry(ctx, run, target, edits)
}

func TestExplicitRetryStillRunningAndStopRace(t *testing.T) {
	t.Run("live process", func(t *testing.T) {
		s, runtime, _, _, _, _ := localFlow(t, `/bin/sleep 60`)
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil || len(runs) != 1 {
			t.Fatalf("runs: %+v %v", runs, err)
		}
		before := runs[0]
		if _, err := runtime.Workflow.Transition(context.Background(), before.ID, workflow.Request{Trigger: workflow.OperationFailed, Failure: &fault.Error{Code: "internal.test", Message: "Inspect running work"}}); err != nil {
			t.Fatal(err)
		}
		selected, err := s.Retry(context.Background(), before.ID)
		var coded *fault.Error
		if !errors.As(err, &coded) || coded.Code != "retry.process_running" || selected.State != workflow.NeedsAttention {
			t.Fatalf("relaunched live work: %+v %v", selected, err)
		}
		if err := s.Stop(context.Background(), before.ID); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("duplicate retry and stop", func(t *testing.T) {
		_, runtime, api, _, cfg, r := localFlow(t, loopScript(disputedReport))
		cfg.MaxRounds = 1
		s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
		if err != nil {
			t.Fatal(err)
		}
		before := finish(t, s, workflow.NeedsAttention, workflow.Review)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); s.Retry(context.Background(), before.ID) }()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Stop(context.Background(), before.ID); err != nil {
				t.Error(err)
			}
		}()
		wg.Wait()
		after, err := runtime.Workflow.Get(context.Background(), before.ID)
		if err != nil || after.State != workflow.Stopped || len(after.Retries) > 1 {
			t.Fatalf("racing operations: %+v %v", after, err)
		}
		var attempts int
		if err := runtime.DB.QueryRow("SELECT count(*) FROM phase_attempts WHERE run_id=?", before.ID).Scan(&attempts); err != nil || attempts != 2 {
			t.Fatalf("race launched work: %d %v", attempts, err)
		}
	})
}

func TestExplicitRetryDashboardAndCLIAPIShareScheduler(t *testing.T) {
	for _, route := range []string{"/runs/", "/api/runs/"} {
		for _, state := range []workflow.State{workflow.NeedsAttention, workflow.Failed} {
			t.Run(route+string(state), func(t *testing.T) {
				_, runtime, api, _, cfg, r := localFlow(t, loopScript(disputedReport))
				cfg.MaxRounds = 1
				s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
				if err != nil {
					t.Fatal(err)
				}
				before := finish(t, s, workflow.NeedsAttention, workflow.Review)
				if state == workflow.Failed {
					if _, err := runtime.Workflow.Transition(context.Background(), before.ID, workflow.Request{Trigger: workflow.InternalFailure, Failure: &fault.Error{Code: "review.max_rounds_exceeded", Message: "No review rounds remain"}}); err != nil {
						t.Fatal(err)
					}
				}
				server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
				if err != nil {
					t.Fatal(err)
				}
				statusRecorder := httptest.NewRecorder()
				server.ServeHTTP(statusRecorder, httptest.NewRequest("GET", "http://127.0.0.1:7331/api/status", nil))
				var status web.Status
				if err := json.Unmarshal(statusRecorder.Body.Bytes(), &status); err != nil {
					t.Fatal(err)
				}
				page := httptest.NewRecorder()
				server.ServeHTTP(page, httptest.NewRequest("GET", "http://127.0.0.1:7331/runs/"+before.ID, nil))
				if page.Code != 200 || !strings.Contains(page.Body.String(), "Retry run") {
					t.Fatalf("retry absent in %s: %s", state, page.Body.String())
				}
				for _, invalid := range []string{"host", "origin", "token", "cross-site"} {
					request := httptest.NewRequest("POST", "http://127.0.0.1:7331"+route+before.ID+"/retry", nil)
					request.Header.Set("Origin", "http://127.0.0.1:7331")
					request.Header.Set("X-CSRF-Token", status.Token)
					switch invalid {
					case "host":
						request.Host = "evil.example:7331"
					case "origin":
						request.Header.Set("Origin", "https://evil.example")
					case "token":
						request.Header.Del("X-CSRF-Token")
					case "cross-site":
						request.Header.Set("Sec-Fetch-Site", "cross-site")
					}
					response := httptest.NewRecorder()
					server.ServeHTTP(response, request)
					after, err := runtime.Workflow.Get(context.Background(), before.ID)
					if response.Code != 403 || err != nil || after.State != state || len(after.Retries) != 0 {
						t.Fatalf("unauthorized retry changed run: %d %+v %v", response.Code, after, err)
					}
				}
				for range 2 {
					request := httptest.NewRequest("POST", "http://127.0.0.1:7331"+route+before.ID+"/retry", nil)
					request.Header.Set("Origin", "http://127.0.0.1:7331")
					request.Header.Set("X-CSRF-Token", status.Token)
					response := httptest.NewRecorder()
					server.ServeHTTP(response, request)
					if route == "/runs/" {
						if response.Code != 303 || response.Header().Get("Location") != "/runs/"+before.ID {
							t.Fatalf("dashboard retry: %d %s", response.Code, response.Body.String())
						}
					} else {
						var selected workflow.Run
						if err := json.Unmarshal(response.Body.Bytes(), &selected); err != nil || response.Code != 200 || selected.ID != before.ID || selected.Phase != workflow.Fix {
							t.Fatalf("CLI API retry: %+v %d %v", selected, response.Code, err)
						}
					}
				}
				after, err := runtime.Workflow.Get(context.Background(), before.ID)
				if err != nil || len(after.Retries) != 1 || after.Retries[0].GrantedRound != 2 || after.State != workflow.Active || after.Phase != workflow.Fix {
					t.Fatalf("handler duplicated orchestration: %+v %v", after, err)
				}
				page = httptest.NewRecorder()
				server.ServeHTTP(page, httptest.NewRequest("GET", "http://127.0.0.1:7331/runs/"+before.ID, nil))
				for _, text := range []string{"Additional review round granted: 2", "Selected ACTIVE / fix", "run.retry_requested", "\"trigger\":\"retry\""} {
					if page.Code != 200 || !strings.Contains(html.UnescapeString(page.Body.String()), text) {
						t.Fatalf("retry detail missing %q: %s", text, page.Body.String())
					}
				}
			})
		}
	}
}

func TestExplicitRetryCIOutcomes(t *testing.T) {
	for _, outcome := range []string{"success", "pending", "unknown", "failure", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			_, runtime, base, _, cfg, r := localFlow(t, successfulScript)
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			api := &ciGitHub{fakeGitHub: base, applyReady: true, evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "queued"}}}}
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			before := finish(t, s, workflow.WaitingForCI, workflow.Review)
			now = before.CI.Deadline
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := workflow.WaitingForCI
			switch outcome {
			case "success":
				api.evidence.Checks[0].Status = "completed"
				api.evidence.Checks[0].Conclusion = "success"
				want = workflow.ReadyToMerge
			case "unknown":
				api.queryError = errors.New("CI temporarily unavailable")
			case "failure":
				api.evidence.Checks[0].Status = "completed"
				api.evidence.Checks[0].Conclusion = "failure"
				want = workflow.Active
			case "cancelled":
				api.evidence.Checks[0].Status = "completed"
				api.evidence.Checks[0].Conclusion = "cancelled"
				want = workflow.NeedsAttention
			}
			selected, err := s.Retry(context.Background(), before.ID)
			if err != nil || selected.State != want || selected.ReviewRound != before.ReviewRound || selected.Retries[0].GrantedRound != 0 {
				t.Fatalf("CI retry: %+v %v", selected, err)
			}
			if want != workflow.Active && selected.ApprovedSHA != before.ApprovedSHA {
				t.Fatal("lost current approval")
			}
			if want == workflow.Active && (selected.Phase != workflow.Fix || selected.ApprovedSHA != "") {
				t.Fatal("repair retained approval")
			}
			var attempts int
			if err := runtime.DB.QueryRow("SELECT count(*) FROM phase_attempts WHERE run_id=?", before.ID).Scan(&attempts); err != nil || attempts != 2 {
				t.Fatalf("CI retry launched work: %d %v", attempts, err)
			}
		})
	}
}

func TestExplicitRetryGrantsCIRepairRound(t *testing.T) {
	_, runtime, base, _, cfg, r := localFlow(t, ciFixScript(`printf repaired > feature.txt; `+ciFixedReport))
	cfg.MaxRounds = 1
	api := &ciGitHub{fakeGitHub: base, applyReady: true, evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "failure"}}}}
	deps := scheduler.Dependencies{GitHub: ciBranchGitHub{api}, Runner: r}
	s, err := scheduler.New(cfg, schedulerResources(runtime), deps)
	if err != nil {
		t.Fatal(err)
	}
	before := finish(t, s, workflow.NeedsAttention, workflow.Review)
	if before.LastErrorCode != "review.max_rounds_exceeded" {
		t.Fatalf("fixture: %+v", before)
	}
	selected, err := s.Retry(context.Background(), before.ID)
	if err != nil || selected.State != workflow.Active || selected.Phase != workflow.Fix || selected.Retries[0].GrantedRound != 2 || selected.ApprovedSHA != "" {
		t.Fatalf("CI exhaustion retry: %+v %v", selected, err)
	}
	if _, err := s.Retry(context.Background(), before.ID); err != nil {
		t.Fatal(err)
	}
	api.evidence.Checks[0].Conclusion = "success"
	s, err = scheduler.New(cfg, schedulerResources(runtime), deps)
	if err != nil {
		t.Fatal(err)
	}
	after := finish(t, s, workflow.ReadyToMerge, workflow.Review)
	if after.ReviewRound != 2 || len(after.Retries) != 1 || len(after.FixHistory) != 1 || len(after.ReviewHistory) != 2 || !after.Review.Accepted {
		t.Fatalf("CI repair was not independently reviewed: %+v", after)
	}
}

func TestExplicitRetryRecognizesCompletedImplementation(t *testing.T) {
	_, runtime, base, _, cfg, r := localFlow(t, successfulScript)
	api := &retryPublicationGitHub{fakeGitHub: base, unavailable: true}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	before := finish(t, s, workflow.NeedsAttention, workflow.Implement)
	if before.Implementer.Status != "succeeded" {
		t.Fatalf("fixture: %+v", before)
	}
	api.unavailable = false
	selected, err := s.Retry(context.Background(), before.ID)
	if err != nil || selected.Retries[0].AttemptFrom != 0 {
		t.Fatalf("completed work authorized another attempt: %+v %v", selected, err)
	}
	after := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if after.ID != before.ID || after.Implementer.Attempt != 1 || api.creations != 1 {
		t.Fatalf("replayed completed work: %+v", after)
	}
}

type retryPublicationGitHub struct {
	*fakeGitHub
	unavailable bool
}

func (f *retryPublicationGitHub) CreateDraftPullRequest(ctx context.Context, repo, branch, base string, content github.PullRequestContent) (*github.PullRequest, error) {
	if f.unavailable {
		return nil, &fault.Error{Code: "github.offline", Message: "Publication temporarily unavailable"}
	}
	return f.fakeGitHub.CreateDraftPullRequest(ctx, repo, branch, base, content)
}

func TestExplicitRetryRejectionDoesNotExposeUnderlyingCause(t *testing.T) {
	_, runtime, base, _, cfg, r := localFlow(t, successfulScript)
	git := &retryErrorGit{Manager: managedgit.New(runtime.Workspace)}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: base, Git: git, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	before := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if _, err := runtime.Workflow.Transition(context.Background(), before.ID, workflow.Request{Trigger: workflow.OperationFailed, Failure: &fault.Error{Code: "internal.test", Message: "Inspect preserved work"}}); err != nil {
		t.Fatal(err)
	}
	git.failure = errors.New("SECRET_UNDERLYING_CAUSE")
	selected, err := s.Retry(context.Background(), before.ID)
	if err == nil {
		t.Fatal("fixture did not reject retry")
	}
	snapshot, _ := json.Marshal(selected)
	if strings.Contains(string(snapshot), "SECRET_UNDERLYING_CAUSE") {
		t.Fatalf("retry snapshot exposed underlying cause: %s", snapshot)
	}
	history, err := runtime.Events.History(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range history {
		if event.Type == "run.retry_rejected" && strings.Contains(string(event.Payload), "SECRET_UNDERLYING_CAUSE") {
			t.Fatal("SSE/history exposed underlying cause")
		}
	}
}

type retryErrorGit struct {
	*managedgit.Manager
	failure error
}

func (g *retryErrorGit) InspectRetry(ctx context.Context, run managedgit.Run, target string, edits bool) (string, error) {
	if g.failure != nil {
		return "", g.failure
	}
	return g.Manager.InspectRetry(ctx, run, target, edits)
}

func TestExplicitRetryRefreshesFailedCIRepair(t *testing.T) {
	for _, outcome := range []string{"success", "pending", "unknown", "cancelled", "failure"} {
		t.Run(outcome, func(t *testing.T) {
			_, runtime, base, _, cfg, r := localFlow(t, ciFixScript(`exit 1`))
			api := &ciGitHub{fakeGitHub: base, applyReady: true, evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "failure"}}}}
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			before := finish(t, s, workflow.NeedsAttention, workflow.Fix)
			if before.ApprovedSHA != "" || before.Fix.Attempt != 1 {
				t.Fatalf("fixture: %+v", before)
			}
			reads := 0
			api.afterEvidence = func() { reads++ }
			want := workflow.WaitingForCI
			switch outcome {
			case "success":
				api.evidence.Checks[0].Conclusion = "success"
				want = workflow.ReadyToMerge
			case "pending":
				api.evidence.Checks[0].Status = "queued"
			case "unknown":
				api.queryError = errors.New("CI query unavailable")
			case "cancelled":
				api.evidence.Checks[0].Conclusion = "cancelled"
				want = workflow.NeedsAttention
			case "failure":
				want = workflow.Active
			}
			selected, err := s.Retry(context.Background(), before.ID)
			if err != nil || reads == 0 || selected.State != want {
				t.Fatalf("stale CI repair: reads=%d state=%s want=%s err=%v", reads, selected.State, want, err)
			}
			if outcome == "failure" && (selected.Phase != workflow.Fix || selected.Retries[0].AttemptFrom != 2 || selected.ApprovedSHA != "") {
				t.Fatalf("repair attempt not authorized: %+v", selected)
			}
			var attempts int
			if err := runtime.DB.QueryRow("SELECT count(*) FROM phase_attempts WHERE run_id=?", before.ID).Scan(&attempts); err != nil || attempts != 3 {
				t.Fatalf("retry launched an agent: %d %v", attempts, err)
			}
		})
	}
}

func TestExplicitRetryCILabelFailureDoesNotBypassDeadline(t *testing.T) {
	_, runtime, base, _, cfg, r := localFlow(t, successfulScript)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	api := &ciGitHub{fakeGitHub: base, evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "queued"}}}}
	deps := scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }}
	s, err := scheduler.New(cfg, schedulerResources(runtime), deps)
	if err != nil {
		t.Fatal(err)
	}
	before := finish(t, s, workflow.WaitingForCI, workflow.Review)
	now = before.CI.Deadline
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	api.mutate = func(action, repo string, n int, label string) error {
		if action == "add" && label == "agent-running" {
			return errors.New("GitHub labels unavailable")
		}
		return nil
	}
	selected, err := s.Retry(context.Background(), before.ID)
	if err == nil || selected.State != workflow.WaitingForCI || selected.Retries[0].Pending {
		t.Fatalf("label failure lost selection: %+v %v", selected, err)
	}
	s, err = scheduler.New(cfg, schedulerResources(runtime), deps)
	if err != nil {
		t.Fatal(err)
	}
	now = selected.CI.Deadline
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := runtime.Workflow.Get(context.Background(), before.ID)
	if err != nil || after.State != workflow.NeedsAttention || after.LastErrorCode != "ci.wait_timeout" || len(after.Retries) != 1 || after.ReviewRound != before.ReviewRound {
		t.Fatalf("labels bypassed deadline: %+v %v", after, err)
	}
}
