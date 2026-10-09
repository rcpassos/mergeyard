package scheduler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/rcpassos/mergeyard/internal/web"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type creditHarness struct {
	*classifiedHarness
	detection bool
}

func (h *creditHarness) Capabilities() harness.HarnessCapabilities {
	c := h.HarnessAdapter.Capabilities()
	c.CreditExhaustionDetection = h.detection
	return c
}

func TestCreditRetrySelectedRunRecoversWithoutReset(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			_, runtime, api, _, cfg, r := pairingFlow(t, agent, agent, disputedReport)
			replacePhaseScript(t, cfg, workflow.Implement, `case "$*" in *implement-0-1*) printf partial > partial.txt; exit 1;; esac
`+successfulScript)
			adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
			if agent == "codex" {
				adapter = harness.NewCodex(cfg.Agents.Codex)
			}
			h := &creditHarness{classifiedHarness: &classifiedHarness{HarnessAdapter: adapter, outcome: harness.FailureClassification{Kind: harness.CreditsExhausted, Source: "classified-fixture"}}, detection: true}
			now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
			deps := scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{agent: h}}
			s, err := scheduler.New(cfg, schedulerResources(runtime), deps)
			if err != nil {
				t.Fatal(err)
			}
			attention := finish(t, s, workflow.NeedsAttention, workflow.Implement)
			if attention.LastErrorCode != "harness.credits_exhausted" || attention.HarnessWait == nil || !attention.HarnessWait.ResetAt.IsZero() {
				t.Fatalf("credit attention: %+v", attention)
			}
			availability, err := s.HarnessAvailability(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range availability {
				if v.Harness == agent && (v.Available || !v.ResetAt.IsZero()) {
					t.Fatalf("credit block: %+v", v)
				}
			}
			server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			for _, enabled := range []bool{false, true} {
				h.detection = enabled
				response := httptest.NewRecorder()
				server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/runs/"+attention.ID, nil))
				body := response.Body.String()
				if response.Code != 200 || !strings.Contains(body, "no scheduled reset") || strings.Contains(body, "0001-01-01") || strings.Contains(body, ">Retry run<") != enabled {
					t.Fatalf("credit controls capability=%v: %d %s", enabled, response.Code, body)
				}
			}
			h.detection = false
			assertCreditConflict(t, server, attention.ID, "harness.credit_detection_unavailable", "Credit recovery is unavailable")
			h.detection = true
			now = now.Add(365 * 24 * time.Hour)
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			selected, err := s.Retry(context.Background(), attention.ID)
			if err != nil || selected.State != workflow.Active {
				t.Fatalf("Retry: %+v %v", selected, err)
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/api/status", nil))
			var snapshot web.Status
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &snapshot) != nil || len(snapshot.Runs[0].CreditProbes) != 1 || snapshot.Runs[0].CreditProbes[0].Status != "reserved" || strings.Contains(response.Body.String(), "0001-01-01") {
				t.Fatalf("reserved probe status: %d %s", response.Code, response.Body.String())
			}
			s, err = scheduler.New(cfg, schedulerResources(runtime), deps)
			if err != nil {
				t.Fatal(err)
			}
			done := finish(t, s, workflow.Active, workflow.Review)
			if done.Implementer.Attempt != 2 || done.Implementer.SessionID != attention.Implementer.SessionID {
				t.Fatalf("continuity: %+v", done)
			}
			availability, err = s.HarnessAvailability(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range availability {
				if v.Harness == agent && !v.Available {
					t.Fatalf("recovery: %+v", v)
				}
			}
		})
	}
}

func TestCreditProbeRequiresModelCompletionAndHonorsCapability(t *testing.T) {
	for _, response := range []string{"blocked", "failed", "incomplete", "credits", "unsupported"} {
		t.Run(response, func(t *testing.T) {
			_, rt, api, _, cfg, r := localFlow(t, `exit 1`)
			h := &creditHarness{classifiedHarness: &classifiedHarness{HarnessAdapter: harness.NewClaude(cfg.Agents.Claude), outcome: harness.FailureClassification{Kind: harness.CreditsExhausted}}, detection: response != "unsupported"}
			deps := scheduler.Dependencies{GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{"claude": h}}
			s, err := scheduler.New(cfg, schedulerResources(rt), deps)
			if err != nil {
				t.Fatal(err)
			}
			run := finish(t, s, workflow.NeedsAttention, workflow.Implement)
			if response == "unsupported" {
				if _, err := s.Retry(context.Background(), run.ID); err == nil {
					t.Fatal("offered unsupported credit recovery")
				}
				return
			}
			script := `printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"schema_version":1,"status":"` + response + `","summary":"Task needs attention"}}'`
			if response == "incomplete" {
				script = `printf '%s\n' '{"type":"system","subtype":"init","session_id":"login-only"}'; exit 1`
				h.outcome.Kind = harness.Ordinary
			}
			if response == "credits" {
				script = `exit 1`
			}
			replacePhaseScript(t, cfg, workflow.Implement, script)
			if _, err := s.Retry(context.Background(), run.ID); err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				got, err := rt.Workflow.Get(context.Background(), run.ID)
				if err != nil {
					t.Fatal(err)
				}
				return got.State == workflow.NeedsAttention || got.State == workflow.WaitingForHarness
			})
			states, err := s.HarnessAvailability(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range states {
				if v.Harness == "claude" && (v.Available != (response == "blocked" || response == "failed") || v.ProbeID != "") {
					t.Fatalf("proof %s: %+v", response, v)
				}
			}
			got, err := rt.Workflow.Get(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.CreditProbes) != 1 {
				t.Fatalf("probe history: %+v", got)
			}
		})
	}
}

func TestCreditProbeConcurrentSelectionStopAndWaitingRetry(t *testing.T) {
	_, rt, api, _, cfg, r := localFlow(t, `exit 1`)
	cfg.Concurrency = 2
	cfg.Repositories[0].Concurrency = 2
	api.issues["owner/repo"] = append(api.issues["owner/repo"], ready(8))
	h := &creditHarness{classifiedHarness: &classifiedHarness{HarnessAdapter: harness.NewClaude(cfg.Agents.Claude), outcomes: []harness.FailureClassification{{Kind: harness.CreditsExhausted}, {Kind: harness.TemporaryLimit}}}, detection: true}
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	deps := scheduler.Dependencies{Now: func() time.Time { return now }, GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{"claude": h}}
	s, err := scheduler.New(cfg, schedulerResources(rt), deps)
	if err != nil {
		t.Fatal(err)
	}
	var runs []workflow.Run
	waitFor(t, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err = s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return len(runs) == 2 && ((runs[0].State == workflow.NeedsAttention && runs[1].State == workflow.WaitingForHarness) || (runs[1].State == workflow.NeedsAttention && runs[0].State == workflow.WaitingForHarness))
	})
	for _, run := range runs {
		if !run.RetryEligible || run.HarnessWait.Reason != "credits_exhausted" || !run.HarnessWait.ResetAt.Equal(now.Add(cfg.UsageLimits.Cooldown)) {
			t.Fatalf("affected run: %+v", run)
		}
	}
	type selection struct {
		run workflow.Run
		err error
	}
	results := make(chan selection, 2)
	for _, run := range runs {
		go func(id string) { v, err := s.Retry(context.Background(), id); results <- selection{v, err} }(run.ID)
	}
	first, second := <-results, <-results
	if (first.err == nil) == (second.err == nil) {
		t.Fatalf("exclusive selection: %v %v", first.err, second.err)
	}
	winner := first.run
	if first.err != nil {
		winner = second.run
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	busy, err := s.Runs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range busy {
		if run.RetryEligible {
			t.Fatalf("busy harness offered Retry: %+v", run)
		}
		page := httptest.NewRecorder()
		server.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil))
		if page.Code != 200 || strings.Contains(page.Body.String(), ">Retry run<") {
			t.Fatalf("busy probe offered Retry: %d %s", page.Code, page.Body.String())
		}
		if run.ID != winner.ID {
			assertCreditConflict(t, server, run.ID, "harness.probe_unavailable", "refresh before retrying")
		}
	}
	for range 2 {
		v, err := s.Retry(context.Background(), winner.ID)
		if err != nil || len(v.CreditProbes) != 1 {
			t.Fatalf("duplicate reservation: %+v %v", v, err)
		}
	}
	// Stop before launch releases authorization even if label bookkeeping fails.
	api.mutate = func(action, repo string, n int, label string) error {
		if action == "remove" {
			return fmt.Errorf("labels unavailable")
		}
		return nil
	}
	if err := s.Stop(context.Background(), winner.ID); err == nil {
		t.Fatal("expected label failure")
	}
	states, err := s.HarnessAvailability(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range states {
		if v.Harness == "claude" && (v.Available || v.ProbeID != "") {
			t.Fatalf("Stop released block instead of reservation: %+v", v)
		}
	}
	api.mutate = nil
	if err := s.Stop(context.Background(), winner.ID); err != nil {
		t.Fatal(err)
	}
	loser := runs[0]
	if loser.ID == winner.ID {
		loser = runs[1]
	}
	now = now.Add(time.Hour)
	replacePhaseScript(t, cfg, workflow.Implement, `sleep 60; `+successfulScript)
	if _, err := s.Retry(context.Background(), loser.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		states, err = s.HarnessAvailability(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range states {
			if v.Harness == "claude" {
				return v.ProbeAttemptID != ""
			}
		}
		return false
	})
	if err := s.Stop(context.Background(), loser.ID); err != nil {
		t.Fatal(err)
	}
	states, err = s.HarnessAvailability(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range states {
		if v.Harness == "claude" && (v.Available || v.ProbeID != "") {
			t.Fatalf("Stop after launch: %+v", v)
		}
	}
}

func TestCreditRecoveryAcrossReviewAndFixPairings(t *testing.T) {
	for _, implementer := range []string{"claude", "codex"} {
		for _, reviewer := range []string{"claude", "codex"} {
			for _, phase := range []workflow.Phase{workflow.Review, workflow.Fix} {
				t.Run(implementer+"/"+reviewer+"/"+string(phase), func(t *testing.T) {
					_, rt, api, remote, cfg, r := pairingFlow(t, implementer, reviewer, `printf fixed > feature.txt; `+fixedReport)
					script := loopScript(`case "$*" in *fix-1-1*) printf partial > partial.txt; exit 1;; esac
printf fixed > feature.txt; ` + fixedReport)
					agent := implementer
					if phase == workflow.Review {
						agent = reviewer
						script = reviewScript(`case "$*" in *review-1-1*) printf tamper > feature.txt; printf reviewer > reviewer.txt; exit 1;; esac
` + approvedReview)
					}
					replacePhaseScript(t, cfg, phase, script)
					adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
					if agent == "codex" {
						adapter = harness.NewCodex(cfg.Agents.Codex)
					}
					h := &creditHarness{classifiedHarness: &classifiedHarness{HarnessAdapter: adapter, outcome: harness.FailureClassification{Kind: harness.CreditsExhausted}}, detection: true}
					deps := scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r, Harnesses: map[string]harness.HarnessAdapter{agent: h}}
					s, err := scheduler.New(cfg, schedulerResources(rt), deps)
					if err != nil {
						t.Fatal(err)
					}
					attention := finish(t, s, workflow.NeedsAttention, phase)
					if attention.LastErrorCode != "harness.credits_exhausted" || (phase == workflow.Review && (!attention.Review.Restored || !attention.Review.Contaminated || attention.Review.Accepted)) {
						t.Fatalf("interrupted phase: %+v", attention)
					}
					if _, err := s.Retry(context.Background(), attention.ID); err != nil {
						t.Fatal(err)
					}
					done := finish(t, s, workflow.WaitingForCI, workflow.Review)
					if phase == workflow.Review && (done.ReviewRound != 1 || done.Review.Attempt != 2 || done.Review.SessionID != attention.Review.SessionID) {
						t.Fatalf("review continuity: %+v", done)
					}
					if phase == workflow.Fix && (done.Fix.Attempt != 2 || done.ReviewRound != 2 || done.Implementer.SessionID != attention.Implementer.SessionID) {
						t.Fatalf("fix continuity: %+v", done)
					}
					if len(done.CreditProbes) != 1 || done.CreditProbes[0].Status != "recovered" || api.creations != 1 || gitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7") == "0" {
						t.Fatalf("selected recovery: %+v", done)
					}
				})
			}
		}
	}
}

func TestOlderProbeCompletionCannotClearNewCreditRestriction(t *testing.T) {
	for _, kind := range []harness.FailureKind{harness.CreditsExhausted, harness.TemporaryLimit} {
		t.Run(string(kind), func(t *testing.T) { olderProbeCompletion(t, kind) })
	}
}
func olderProbeCompletion(t *testing.T, kind harness.FailureKind) {
	marker := filepath.Join(t.TempDir(), "new-restriction")
	proof := filepath.Join(t.TempDir(), "probe-completion")
	script := `case "$(git branch --show-current)" in mergeyard/issue-7) exit 1;; *) while [ ! -f '` + marker + `' ]; do sleep 0.05; done; exit 1;; esac`
	_, rt, api, _, cfg, r := localFlow(t, script)
	cfg.Concurrency = 2
	cfg.Repositories[0].Concurrency = 2
	api.issues["owner/repo"] = append(api.issues["owner/repo"], ready(8))
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	h := &creditHarness{classifiedHarness: &classifiedHarness{HarnessAdapter: harness.NewClaude(cfg.Agents.Claude), outcomes: []harness.FailureClassification{{Kind: harness.CreditsExhausted}, {Kind: kind, ResetAt: reset}}}, detection: true}
	deps := scheduler.Dependencies{Now: func() time.Time { return now }, GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{"claude": h}}
	s, err := scheduler.New(cfg, schedulerResources(rt), deps)
	if err != nil {
		t.Fatal(err)
	}
	var selected workflow.Run
	waitFor(t, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range runs {
			if run.IssueNumber == 7 && run.State == workflow.NeedsAttention {
				selected = run
				return true
			}
		}
		return false
	})
	replacePhaseScript(t, cfg, workflow.Implement, `while [ ! -f '`+proof+`' ]; do sleep 0.05; done
`+successfulScript)
	if _, err := s.Retry(context.Background(), selected.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		states, err := s.HarnessAvailability(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range states {
			if v.Harness == "claude" {
				return v.ProbeAttemptID != ""
			}
		}
		return false
	})
	if err := os.WriteFile(marker, []byte("release"), 0600); err != nil {
		t.Fatal(err)
	}
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
				if kind == harness.TemporaryLimit {
					return run.State == workflow.WaitingForHarness
				}
				return run.State == workflow.NeedsAttention
			}
		}
		return false
	})
	runs, err := s.Runs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if run.IssueNumber == 8 {
			if _, err := s.Retry(context.Background(), run.ID); err == nil {
				t.Fatal("new restriction allowed another probe while older execution still runs")
			}
		}
	}
	if err := os.WriteFile(proof, []byte("complete"), 0600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		run, err := rt.Workflow.Get(context.Background(), selected.ID)
		if err != nil {
			t.Fatal(err)
		}
		return run.Phase == workflow.Review
	})
	s, err = scheduler.New(cfg, schedulerResources(rt), deps)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := s.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	states, err := s.HarnessAvailability(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range states {
		if v.Harness == "claude" && (v.Available || v.ProbeID != "" || v.Reason != string(kind) || (kind == harness.TemporaryLimit && !v.ResetAt.Equal(reset))) {
			t.Fatalf("old proof cleared newer restriction: %+v", v)
		}
	}
	if kind == harness.TemporaryLimit {
		recovered, err := rt.Workflow.Get(context.Background(), selected.ID)
		if err != nil || recovered.CreditProbes[0].Status != "recovered" {
			t.Fatalf("credit proof lost alongside timed gate: %+v %v", recovered, err)
		}
		now = reset
		states, err = s.HarnessAvailability(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range states {
			if v.Harness == "claude" && !v.Available {
				t.Fatalf("remaining timed limit did not expire: %+v", v)
			}
		}
	}
	history, err := rt.Events.History(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range history {
		if event.Type == "harness.available" {
			t.Fatal("stale availability event")
		}
	}
}

func TestCreditRetryReconcilesWorkOnOtherHarness(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			other := "codex"
			if agent == "codex" {
				other = "claude"
			}
			_, rt, api, _, cfg, r := pairingFlow(t, agent, other, disputedReport)
			replacePhaseScript(t, cfg, workflow.Fix, `printf partial > partial.txt; exit 1`)
			adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
			if agent == "codex" {
				adapter = harness.NewCodex(cfg.Agents.Codex)
			}
			h := &creditHarness{classifiedHarness: &classifiedHarness{HarnessAdapter: adapter, outcome: harness.FailureClassification{Kind: harness.CreditsExhausted}}, detection: true}
			s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{agent: h}})
			if err != nil {
				t.Fatal(err)
			}
			run := finish(t, s, workflow.NeedsAttention, workflow.Implement)
			// External work has published a PR while the automated implementer waits.
			paths, err := filepath.Glob(filepath.Join(rt.Workspace.Root, "worktrees", "*", "*"))
			if err != nil || len(paths) != 1 {
				t.Fatalf("worktrees %v %v", paths, err)
			}
			path := paths[0]
			gitCommand(t, path, "add", ".")
			gitCommand(t, path, "commit", "-m", "User completed partial work")
			gitCommand(t, path, "push", "origin", "HEAD")
			pr := &github.PullRequest{Number: 101, State: github.Open, Draft: true}
			pr.Head.Ref = "mergeyard/issue-7"
			pr.Head.SHA = gitCommand(t, path, "rev-parse", "HEAD")
			pr.Head.Repo.FullName = "owner/repo"
			pr.Base.Ref = "main"
			api.prs = map[string]*github.PullRequest{pr.Head.Ref: pr}
			selected, err := s.Retry(context.Background(), run.ID)
			if err != nil || selected.State != workflow.Active || selected.Phase != workflow.Review || len(selected.CreditProbes) != 0 {
				t.Fatalf("reconciled Retry: %+v %v", selected, err)
			}
			waitFor(t, func() bool {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				got, err := rt.Workflow.Get(context.Background(), run.ID)
				if err != nil {
					t.Fatal(err)
				}
				return got.State == workflow.WaitingForHarness && got.Phase == workflow.Fix && got.Review != nil && got.Review.Agent == other && got.Review.Status == "succeeded"
			})
			states, err := s.HarnessAvailability(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range states {
				if v.Harness == agent && (v.Available || v.ProbeID != "") {
					t.Fatalf("other harness cleared credit block: %+v", v)
				}
			}
			if err := s.Stop(context.Background(), run.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCreditRetryAfterTemporaryProbeWaitExhaustion(t *testing.T) {
	_, rt, api, _, cfg, r := localFlow(t, `exit 1`)
	cfg.UsageLimits.MaxWaits = 2
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	reported := now.Add(time.Hour)
	h := &creditHarness{classifiedHarness: &classifiedHarness{HarnessAdapter: harness.NewClaude(cfg.Agents.Claude), outcomes: []harness.FailureClassification{{Kind: harness.CreditsExhausted}, {Kind: harness.TemporaryLimit, ResetAt: reported}}, outcome: harness.FailureClassification{Kind: harness.TemporaryLimit}}, detection: true}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{"claude": h}})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.NeedsAttention, workflow.Implement)
	if _, err := s.Retry(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	run = finish(t, s, workflow.WaitingForHarness, workflow.Implement)
	if run.HarnessWait.Consecutive != 1 || !run.HarnessWait.ResetAt.Equal(reported) {
		t.Fatalf("temporary allowance/reset: %+v", run.HarnessWait)
	}
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), "Probe may run after temporary reset") {
		t.Fatalf("credit/temporary diagnostics: %d %s", response.Code, response.Body.String())
	}

	if _, err := s.Retry(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	run, err = rt.Workflow.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != workflow.WaitingForHarness || run.Implementer.Attempt != 2 || len(run.CreditProbes) != 2 || run.CreditProbes[1].Status != "reserved" {
		t.Fatalf("probe bypassed temporary reset: %+v", run)
	}
	again, err := s.Retry(context.Background(), run.ID)
	if err != nil || len(again.CreditProbes) != 2 {
		t.Fatalf("duplicate waiting reservation: %+v %v", again, err)
	}
	now = reported
	run = finish(t, s, workflow.NeedsAttention, workflow.Implement)
	if run.LastErrorCode != "harness.usage_limit_waits_exhausted" || run.HarnessWait.Consecutive != 2 {
		t.Fatalf("wait exhaustion: %+v", run)
	}
	selected, err := s.Retry(context.Background(), run.ID)
	if err != nil || selected.State != workflow.Active || len(selected.CreditProbes) != 3 || selected.CreditProbes[2].Status != "reserved" {
		t.Fatalf("Retry did not reserve credit probe after wait exhaustion: %+v %v", selected, err)
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	run, err = rt.Workflow.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != workflow.WaitingForHarness || run.Implementer.Attempt != 3 {
		t.Fatalf("exhausted wait Retry bypassed cooldown: %+v", run)
	}
	replacePhaseScript(t, cfg, workflow.Implement, successfulScript)
	now = run.HarnessWait.ResetAt
	done := finish(t, s, workflow.Active, workflow.Review)
	if done.Implementer.Attempt != 4 || done.CreditProbes[2].Status != "recovered" {
		t.Fatalf("delayed recovery: %+v", done)
	}
}
