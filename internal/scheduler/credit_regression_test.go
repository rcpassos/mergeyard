package scheduler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/config"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/web"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestCreditRetrySavedImplementationDoesNotReserveProbe(t *testing.T) {
	_, rt, base, _, cfg, r := localFlow(t, `case "$(git branch --show-current)" in mergeyard/issue-7) `+successfulScript+`;; *) exit 1;; esac`)
	cfg.Concurrency = 2
	cfg.Repositories[0].Concurrency = 2
	base.issues["owner/repo"] = append(base.issues["owner/repo"], ready(8))
	api := &retryPublicationGitHub{fakeGitHub: base, unavailable: true}
	h := &creditHarness{classifiedHarness: &classifiedHarness{HarnessAdapter: harness.NewClaude(cfg.Agents.Claude), outcome: harness.FailureClassification{Kind: harness.CreditsExhausted}}, detection: true}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{"claude": h}})
	if err != nil {
		t.Fatal(err)
	}
	var saved workflow.Run
	waitFor(t, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != 2 {
			return false
		}
		for _, run := range runs {
			if run.IssueNumber == 7 {
				saved = run
			}
		}
		return runs[0].State == workflow.NeedsAttention && runs[1].State == workflow.NeedsAttention
	})
	if saved.Implementer.Status != "succeeded" {
		t.Fatalf("saved implementation fixture: %+v", saved)
	}
	api.unavailable = false
	selected, err := s.Retry(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.CreditProbes) != 0 {
		t.Fatalf("publication-only Retry reserved a credit probe: %+v", selected.CreditProbes)
	}
	waitFor(t, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		got, err := rt.Workflow.Get(context.Background(), saved.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.State == workflow.WaitingForHarness && got.Phase == workflow.Review
	})
	states, err := s.HarnessAvailability(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range states {
		if state.Harness == "claude" && (state.Available || state.ProbeID != "") {
			t.Fatalf("publication affected credit block: %+v", state)
		}
	}
}

type discoveryFailureCreditHarness struct {
	*creditHarness
	reject bool
}

func (h *discoveryFailureCreditHarness) DiscoverSession(data []byte) (string, error) {
	if h.reject && bytes.Contains(data, []byte("probe-ready")) {
		return "", &fault.Error{Code: "phase.result_invalid", Message: "Probe session discovery rejected"}
	}
	return h.classifiedHarness.DiscoverSession(data)
}

func TestCreditReviewDiscoveryFailureReleasesProbe(t *testing.T) {
	_, rt, api, _, cfg, r := localFlow(t, reviewScript(`exit 1`))
	cfg.Repositories[0].Reviewer.MaxAttempts = 2
	h := &discoveryFailureCreditHarness{creditHarness: &creditHarness{classifiedHarness: &classifiedHarness{HarnessAdapter: harness.NewClaude(cfg.Agents.Claude), outcome: harness.FailureClassification{Kind: harness.CreditsExhausted}}, detection: true}}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{"claude": h}})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.NeedsAttention, workflow.Review)
	h.reject = true
	replacePhaseScript(t, cfg, workflow.Review, reviewScript(`printf 'probe-ready\n'; sleep 30; exit 1`))
	if _, err := s.Retry(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		run, err = rt.Workflow.Get(context.Background(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		return run.Review.Attempt == 2 && run.Review.Status == "failed"
	})
	if len(run.CreditProbes) != 1 || run.CreditProbes[0].Status != "released" {
		t.Fatalf("ended discovery failure still owns probe: %+v", run.CreditProbes)
	}
	states, err := s.HarnessAvailability(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range states {
		if state.Harness == "claude" && (state.Available || state.ProbeID != "") {
			t.Fatalf("discovery failure changed credit availability: %+v", state)
		}
	}
}

type creditLostPushGit struct {
	*managedgit.Manager
	loseAck bool
}

func (g *creditLostPushGit) PushFix(ctx context.Context, run managedgit.Run, previous, target string) error {
	if err := g.Manager.PushFix(ctx, run, previous, target); err != nil {
		return err
	}
	if g.loseAck {
		return &fault.Error{Code: "git.push_failed", Message: "Push acknowledgement lost"}
	}
	return nil
}

func TestCreditRetrySavedFixDoesNotReserveProbe(t *testing.T) {
	_, rt, api, _, cfg, r := localFlow(t, loopScript(`case "$(git branch --show-current)" in mergeyard/issue-8) exit 1;; esac
printf fixed > feature.txt; `+fixedReport))
	cfg.Concurrency = 2
	cfg.Repositories[0].Concurrency = 2
	// The second run must fail before emitting an implementation result.
	replacePhaseScript(t, cfg, workflow.Implement, `case "$(git branch --show-current)" in mergeyard/issue-8) exit 1;; esac
`+loopScript(`printf fixed > feature.txt; `+fixedReport))
	g := &creditLostPushGit{Manager: managedgit.New(rt.Workspace), loseAck: true}
	h := &creditHarness{classifiedHarness: &classifiedHarness{HarnessAdapter: harness.NewClaude(cfg.Agents.Claude), outcome: harness.FailureClassification{Kind: harness.CreditsExhausted}}, detection: true}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: branchGitHub{api}, Git: g, Runner: r, Harnesses: map[string]harness.HarnessAdapter{"claude": h}})
	if err != nil {
		t.Fatal(err)
	}
	saved := finish(t, s, workflow.NeedsAttention, workflow.Fix)
	if saved.Fix.Status != "succeeded" || saved.Fix.CommitSHA == "" {
		t.Fatalf("saved fix fixture: %+v", saved)
	}
	api.issues["owner/repo"] = append(api.issues["owner/repo"], ready(8))
	waitFor(t, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range runs {
			if run.IssueNumber == 8 {
				return run.State == workflow.NeedsAttention
			}
		}
		return false
	})
	g.loseAck = false
	selected, err := s.Retry(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.CreditProbes) != 0 {
		t.Fatalf("saved fix completion reserved an unused probe: %+v", selected.CreditProbes)
	}
	waitFor(t, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		got, err := rt.Workflow.Get(context.Background(), saved.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.Phase == workflow.Review && got.State == workflow.WaitingForHarness
	})
	states, err := s.HarnessAvailability(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range states {
		if state.Harness == "claude" && (state.Available || state.ProbeID != "") {
			t.Fatalf("saved fix changed credit block: %+v", state)
		}
	}
}

func assertCreditConflict(t *testing.T, server http.Handler, id, code, message string) {
	t.Helper()
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/api/status", nil))
	var status web.Status
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &status) != nil {
		t.Fatalf("status %d %s", response.Code, response.Body.String())
	}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7331/api/runs/"+id+"/retry", nil)
	request.Header.Set("Origin", "http://127.0.0.1:7331")
	request.Header.Set("X-CSRF-Token", status.Token)
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != 409 || response.Header().Get("X-Mergeyard-Error-Code") != code || !strings.Contains(response.Body.String(), message) {
		t.Fatalf("credit conflict lost useful response: %d %s", response.Code, response.Body.String())
	}
}

func TestExpiredTemporaryResetDoesNotDescribeCreditProbeDelay(t *testing.T) {
	_, rt, api, _, cfg, r := localFlow(t, `exit 1`)
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	h := &creditHarness{classifiedHarness: &classifiedHarness{HarnessAdapter: harness.NewClaude(cfg.Agents.Claude), outcomes: []harness.FailureClassification{{Kind: harness.CreditsExhausted}, {Kind: harness.TemporaryLimit, ResetAt: reset}}}, detection: true}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{"claude": h}})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.NeedsAttention, workflow.Implement)
	if _, err := s.Retry(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	finish(t, s, workflow.WaitingForHarness, workflow.Implement)
	now = reset
	states, err := s.HarnessAvailability(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range states {
		if state.Harness == "claude" && (state.Available || !state.ResetAt.IsZero()) {
			t.Fatalf("expired delay presented as future wait: %+v", state)
		}
	}
	limit, err := workflow.LoadHarnessLimit(context.Background(), rt.DB, "claude")
	if err != nil || !limit.ResetAt.Equal(reset) {
		t.Fatalf("expired reset authority lost: %+v %v", limit, err)
	}
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/api/status", nil))
	var snapshot struct {
		Runs []struct {
			HarnessWait map[string]json.RawMessage `json:"harness_wait"`
			History     []workflow.HarnessWait     `json:"harness_wait_history"`
		} `json:"runs"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &snapshot) != nil || len(snapshot.Runs) != 1 {
		t.Fatalf("run JSON: %d %s", response.Code, response.Body.String())
	}
	if _, present := snapshot.Runs[0].HarnessWait["reset_at"]; present {
		t.Fatalf("current credit wait JSON retained expired reset: %s", response.Body.String())
	}
	if len(snapshot.Runs[0].History) != 2 || !snapshot.Runs[0].History[1].ResetAt.Equal(reset) {
		t.Fatalf("observed reset history lost: %+v", snapshot.Runs[0].History)
	}
	response = httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/", nil))
	if response.Code != 200 || strings.Contains(response.Body.String(), "Probe may run after temporary reset") {
		t.Fatalf("expired delay displayed: %d %s", response.Code, response.Body.String())
	}
}

func addBusyAccountProbe(t *testing.T, rt *app.Runtime, base *fakeGitHub, remote string, cfg config.Config, r runner.Runner, agent string) *scheduler.Scheduler {
	t.Helper()
	account := cfg.Repositories[0]
	account.Repo = "owner/account"
	account.Implementer.Agent = agent
	account.Concurrency = 1
	cfg.Concurrency = 2
	cfg.Repositories[0].Concurrency = 1
	cfg.Repositories = append(cfg.Repositories, account)
	base.issues[account.Repo] = []github.Issue{ready(8)}
	gitCommand(t, filepath.Dir(remote), "config", "--file", os.Getenv("GIT_CONFIG_GLOBAL"), "--add", "url."+remote+".insteadOf", "https://github.com/owner/account.git")
	executable := cfg.Agents.Claude.Executable
	adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
	identity := ""
	if agent == "codex" {
		executable = cfg.Agents.Codex.Executable
		adapter = harness.NewCodex(cfg.Agents.Codex)
		identity = codexIdentity + "\n"
	}
	script, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	script = []byte("#!/bin/sh\ncase \"$(git branch --show-current)\" in mergeyard/issue-8)\n" + identity + "exit 1;; esac\n" + strings.TrimPrefix(string(script), "#!/bin/sh\n"))
	if err := os.WriteFile(executable, script, 0700); err != nil {
		t.Fatal(err)
	}
	h := &creditHarness{classifiedHarness: &classifiedHarness{HarnessAdapter: adapter, outcome: harness.FailureClassification{Kind: harness.CreditsExhausted}}, detection: true}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: branchGitHub{base}, Runner: r, Harnesses: map[string]harness.HarnessAdapter{agent: h}})
	if err != nil {
		t.Fatal(err)
	}
	var source workflow.Run
	waitFor(t, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range runs {
			if run.Repository == account.Repo {
				source = run
				return source.State == workflow.NeedsAttention
			}
		}
		return false
	})
	if _, err := s.Retry(context.Background(), source.ID); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCreditRetryEligibilityUsesSelectedHarness(t *testing.T) {
	for _, blocked := range []string{"claude", "codex"} {
		t.Run(blocked, func(t *testing.T) {
			_, rt, api, remote, cfg, r := pairingFlow(t, "claude", "codex", `printf fixed > feature.txt; `+fixedReport)
			cfg.MaxRounds = 1
			s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			primary := finish(t, s, workflow.NeedsAttention, workflow.Review)
			s = addBusyAccountProbe(t, rt, api, remote, cfg, r, blocked)
			runs, err := s.Runs(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, run := range runs {
				if run.ID == primary.ID && run.RetryEligible != (blocked == "codex") {
					t.Fatalf("eligibility used reviewer rather than selected fixer: %+v", run)
				}
			}
			server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			page := httptest.NewRecorder()
			server.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/runs/"+primary.ID, nil))
			if page.Code != 200 || strings.Contains(page.Body.String(), ">Retry run<") != (blocked == "codex") {
				t.Fatalf("selected-harness dashboard action: %d %s", page.Code, page.Body.String())
			}
			if blocked == "codex" {
				selected, err := s.Retry(context.Background(), primary.ID)
				if err != nil || selected.Phase != workflow.Fix || len(selected.CreditProbes) != 0 {
					t.Fatalf("other harness Retry blocked: %+v %v", selected, err)
				}
			}
		})
	}
}

func TestCreditProbeDoesNotHideCIOnlyDashboardRetry(t *testing.T) {
	_, rt, api, remote, cfg, r := localFlow(t, reviewScript(approvedReview))
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	primary := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if _, err := rt.Workflow.Transition(context.Background(), primary.ID, workflow.Request{Trigger: workflow.OperationFailed, Failure: &fault.Error{Code: "ci.wait_timeout", Message: "CI wait timed out"}}); err != nil {
		t.Fatal(err)
	}
	s = addBusyAccountProbe(t, rt, api, remote, cfg, r, "claude")
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	page := httptest.NewRecorder()
	server.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/runs/"+primary.ID, nil))
	if page.Code != 200 || !strings.Contains(page.Body.String(), ">Retry run<") {
		t.Fatalf("saved approval missing from eligibility snapshot: %d %s", page.Code, page.Body.String())
	}
	selected, err := s.Retry(context.Background(), primary.ID)
	if err != nil || selected.State != workflow.WaitingForCI || len(selected.CreditProbes) != 0 {
		t.Fatalf("CI-only retry reserved blocked harness: %+v %v", selected, err)
	}
}

func TestCreditRetryEligibilityIgnoresHistoricalCIHead(t *testing.T) {
	for _, blocked := range []string{"claude", "codex"} {
		t.Run(blocked, func(t *testing.T) {
			_, rt, base, remote, cfg, r := pairingFlow(t, "claude", "codex", disputedReport)
			cfg.MaxRounds = 2
			script := strings.Replace(ciFixScript(`printf repaired > feature.txt; `+ciFixedReport), "*review-*)", "*review-2-*) "+changesReview+";;\n *review-*)", 1)
			replacePhaseScript(t, cfg, workflow.Fix, script)
			replacePhaseScript(t, cfg, workflow.Review, script)
			api := &ciGitHub{fakeGitHub: base, evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "failure"}}}}
			s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: ciBranchGitHub{api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			primary := finish(t, s, workflow.NeedsAttention, workflow.Review)
			if primary.ReviewRound != 2 || primary.CI == nil || primary.CI.CurrentHead == primary.Review.TargetSHA {
				t.Fatalf("historical CI fixture: %+v", primary)
			}
			s = addBusyAccountProbe(t, rt, base, remote, cfg, r, blocked)
			runs, err := s.Runs(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, run := range runs {
				if run.ID == primary.ID && run.RetryEligible != (blocked == "codex") {
					t.Fatalf("historical CI selected wrong harness: %+v", run)
				}
			}
			if blocked == "codex" {
				selected, err := s.Retry(context.Background(), primary.ID)
				if err != nil || selected.Phase != workflow.Fix || len(selected.CreditProbes) != 0 {
					t.Fatalf("historical CI blocked free fixer: %+v %v", selected, err)
				}
			}
		})
	}
}
