package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestHarnessCheckRecoversWithAllRunsStopped(t *testing.T) {
	t.Parallel()

	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			t.Parallel()

			_, rt, api, _, cfg, r := testkit.TmuxPairingFlow(t, agent, agent, testkit.DisputedReport)
			testkit.ReplacePhaseScript(t, cfg, workflow.Implement, `exit 1`)
			adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
			if agent == "codex" {
				adapter = harness.NewCodex(cfg.Agents.Codex)
			}
			h := &testkit.CreditHarness{ClassifiedHarness: &testkit.ClassifiedHarness{HarnessAdapter: adapter, Outcome: harness.FailureClassification{Kind: harness.CreditsExhausted}}, Detection: true}
			deps := scheduler.Dependencies{GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{agent: h}}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
			if err != nil {
				t.Fatal(err)
			}
			run := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
			if err := s.Stop(context.Background(), run.ID); err != nil {
				t.Fatal(err)
			}
			script := `printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"OK"}'`
			if agent == "codex" {
				script = `printf '%s\n' '{"type":"turn.completed"}'`
			}
			executable := cfg.Agents.Claude.Executable
			if agent == "codex" {
				executable = cfg.Agents.Codex.Executable
				script = `case "$*" in *"mcp list --json"*) printf '[]\n'; exit 0;; esac` + "\n" + script
			}
			if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+script), 0700); err != nil {
				t.Fatal(err)
			}
			check, err := s.CheckHarness(context.Background(), agent)
			if err != nil {
				t.Fatal(err)
			}
			if check.Status != "running" {
				t.Fatalf("check: %+v", check)
			}
			// A new scheduler must observe the same physical request after restart.
			s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
			if err != nil {
				t.Fatal(err)
			}
			testkit.WaitFor(t, func() bool {
				if _, err := s.Reconcile(context.Background()); err != nil {
					t.Fatal(err)
				}
				states, err := s.HarnessAvailability(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				for _, v := range states {
					if v.Harness == agent {
						return v.Available
					}
				}
				return false
			})
			saved, err := rt.Workflow.Get(context.Background(), run.ID)
			if err != nil || saved.State != workflow.Stopped || saved.Implementer.Attempt != 1 {
				t.Fatalf("revived stopped run: %+v %v", saved, err)
			}
			states, _ := s.HarnessAvailability(context.Background())
			for _, v := range states {
				if v.Harness == agent && (v.Check == nil || v.Check.ID != check.ID || v.Check.Status != "recovered" || v.ProbeID != "") {
					t.Fatalf("lost check: %+v", v)
				}
			}
		})
	}
}

func TestHarnessCheckKeepsRestrictionsWithoutCompletion(t *testing.T) {
	t.Parallel()

	for _, outcome := range []string{"incomplete", "credits", "temporary", "ordinary"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()

			_, rt, api, _, cfg, r := testkit.TmuxFlow(t, `exit 1`)
			h := &testkit.CreditHarness{ClassifiedHarness: &testkit.ClassifiedHarness{HarnessAdapter: harness.NewClaude(cfg.Agents.Claude), Outcome: harness.FailureClassification{Kind: harness.CreditsExhausted}}, Detection: true}
			now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{"claude": h}})
			if err != nil {
				t.Fatal(err)
			}
			testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
			script := `exit 1`
			switch outcome {
			case "incomplete":
				script = `printf '%s\n' '{"type":"system","subtype":"init"}'`
				h.Outcome.Kind = harness.Ordinary
			case "temporary":
				h.Outcome.Kind = harness.TemporaryLimit
				h.Outcome.ResetAt = now.Add(time.Hour)
			case "ordinary":
				h.Outcome.Kind = harness.Ordinary
			}
			if err := os.WriteFile(cfg.Agents.Claude.Executable, []byte("#!/bin/sh\n"+script), 0700); err != nil {
				t.Fatal(err)
			}
			check, err := s.CheckHarness(context.Background(), "claude")
			if err != nil {
				t.Fatal(err)
			}
			testkit.WaitFor(t, func() bool {
				if _, err := s.Reconcile(context.Background()); err != nil {
					t.Fatal(err)
				}
				states, _ := s.HarnessAvailability(context.Background())
				for _, v := range states {
					if v.Harness == "claude" {
						return v.Check != nil && v.Check.Status == "released"
					}
				}
				return false
			})
			states, _ := s.HarnessAvailability(context.Background())
			for _, v := range states {
				if v.Harness == "claude" {
					if v.Available || v.ProbeID != "" || v.Check.ID != check.ID || v.Check.Result == "" {
						t.Fatalf("unproven availability: %+v", v)
					}
					if outcome == "temporary" {
						if v.CheckEligible || !v.ResetAt.Equal(now.Add(time.Hour)) {
							t.Fatalf("lost timed wait: %+v", v)
						}
						if _, err := s.CheckHarness(context.Background(), "claude"); err == nil {
							t.Fatal("check bypassed timed wait")
						}
						now = now.Add(time.Hour)
						if _, err := s.CheckHarness(context.Background(), "claude"); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
		})
	}
}

func TestHarnessCheckAndRetryShareOneProbe(t *testing.T) {
	t.Parallel()

	for _, actions := range [][]string{{"check", "check"}, {"check", "retry"}} {
		t.Run(strings.Join(actions, "-"), func(t *testing.T) {
			t.Parallel()

			_, rt, api, _, cfg, r := testkit.TmuxFlow(t, `exit 1`)
			h := &testkit.CreditHarness{ClassifiedHarness: &testkit.ClassifiedHarness{HarnessAdapter: harness.NewClaude(cfg.Agents.Claude), Outcome: harness.FailureClassification{Kind: harness.CreditsExhausted}}, Detection: true}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{"claude": h}})
			if err != nil {
				t.Fatal(err)
			}
			run := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
			if err := os.WriteFile(cfg.Agents.Claude.Executable, []byte("#!/bin/sh\nsleep 1\nexit 1\n"), 0700); err != nil {
				t.Fatal(err)
			}
			results := make(chan error, 2)
			start := make(chan struct{})
			for _, action := range actions {
				go func(action string) {
					<-start
					var err error
					if action == "check" {
						_, err = s.CheckHarness(context.Background(), "claude")
					} else {
						_, err = s.Retry(context.Background(), run.ID)
					}
					results <- err
				}(action)
			}
			close(start)
			first, second := <-results, <-results
			if (first == nil) == (second == nil) {
				t.Fatalf("expected one selected probe: %v / %v", first, second)
			}
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			history, err := rt.Events.History(context.Background(), 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			reservations := 0
			for _, event := range history {
				if event.Type == "harness.probe_reserved" {
					reservations++
				}
			}
			if reservations != 1 {
				t.Fatalf("duplicate reservations: %d", reservations)
			}
		})
	}
}

func TestHarnessCheckBrowserControlsAndProtections(t *testing.T) {
	t.Parallel()

	_, rt, api, _, cfg, r := testkit.TmuxFlow(t, `exit 1`)
	h := &testkit.CreditHarness{ClassifiedHarness: &testkit.ClassifiedHarness{HarnessAdapter: harness.NewClaude(cfg.Agents.Claude), Outcome: harness.FailureClassification{Kind: harness.CreditsExhausted}}, Detection: true}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{"claude": h}})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
	if err := s.Stop(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	status := httptest.NewRecorder()
	server.ServeHTTP(status, testkit.ApiRequest(t, rt.Workspace.Root, http.MethodGet, "http://127.0.0.1:7331/api/status"))
	var snapshot web.Status
	if err := json.Unmarshal(status.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	page := func() string {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/", nil))
		if response.Code != 200 {
			t.Fatalf("page: %s", response.Body)
		}
		return response.Body.String()
	}
	h.Detection = false
	if strings.Contains(page(), ">Check availability<") {
		t.Fatal("offered unsupported check")
	}
	h.Detection = true
	body := page()
	if !strings.Contains(body, "claude · Out of credits</span></summary>") || !strings.Contains(body, ">Check availability<") || !strings.Contains(body, "uses account quota") || !strings.Contains(body, "no engineering work") {
		t.Fatalf("missing disclosure: %s", body)
	}
	for _, restriction := range []string{"host", "origin", "csrf", "cross-site", "capability"} {
		request := testkit.DashboardRequest(t, server, http.MethodPost, "http://127.0.0.1:7331/harnesses/claude/check", nil)
		request.Header.Set("Origin", "http://127.0.0.1:7331")
		request.Header.Set("X-CSRF-Token", snapshot.Token)
		switch restriction {
		case "host":
			request.Host = "evil.invalid:7331"
		case "origin":
			request.Header.Set("Origin", "https://evil.invalid")
		case "csrf":
			request.Header.Del("X-CSRF-Token")
		case "cross-site":
			request.Header.Set("Sec-Fetch-Site", "cross-site")
		case "capability":
			h.Detection = false
		}
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != 403 && !(restriction == "capability" && response.Code == 409) {
			t.Fatalf("%s accepted: %d %s", restriction, response.Code, response.Body)
		}
	}
	h.Detection = true
	if err := os.WriteFile(cfg.Agents.Claude.Executable, []byte("#!/bin/sh\nsleep 0.1\nprintf '%s\\n' '{\"type\":\"result\",\"is_error\":false,\"subtype\":\"success\",\"result\":\"OK\"}'"), 0700); err != nil {
		t.Fatal(err)
	}
	request := testkit.DashboardRequest(t, server, http.MethodPost, "http://127.0.0.1:7331/harnesses/claude/check", nil)
	request.Header.Set("Origin", "http://127.0.0.1:7331")
	request.Header.Set("X-CSRF-Token", snapshot.Token)
	request.Header.Set("HX-Request", "true")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "claude · Recovery probe ") || !strings.Contains(response.Body.String(), "independent availability check") || strings.Contains(response.Body.String(), ">Check availability<") {
		t.Fatalf("running UI: %d %s", response.Code, response.Body)
	}
	testkit.WaitFor(t, func() bool {
		if _, err := s.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		return strings.Contains(page(), "Availability check recovered")
	})
	if body := page(); !strings.Contains(body, "claude · Available</span></summary>") {
		t.Fatalf("recovered harness pill: %s", body)
	}
}

func TestHarnessCheckCompletionCannotClearNewRestriction(t *testing.T) {
	t.Parallel()

	for _, kind := range []harness.FailureKind{harness.CreditsExhausted, harness.TemporaryLimit} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()

			marker := filepath.Join(t.TempDir(), "new-restriction")
			proof := filepath.Join(t.TempDir(), "proof")
			script := `case "$PWD" in */harness-checks/*)
 while [ ! -f '` + proof + `' ]; do sleep 0.02; done
 printf '%s\n' '{"type":"result","is_error":false,"subtype":"success","result":"OK"}'
 ;; *)
 case "$(git branch --show-current)" in
 mergeyard/issue-7) exit 1;;
 *) while [ ! -f '` + marker + `' ]; do sleep 0.02; done; exit 1;;
 esac
 ;; esac`
			_, rt, api, _, cfg, r := testkit.TmuxFlow(t, script)
			r = &testkit.DelayedHarnessStartRunner{Runner: r, Gate: filepath.Join(t.TempDir(), "second-launch")}
			cfg.Concurrency = 2
			cfg.Repositories[0].Concurrency = 2
			api.Issues["owner/repo"] = append(api.Issues["owner/repo"], testkit.Ready(8))
			now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
			h := &testkit.CreditHarness{ClassifiedHarness: &testkit.ClassifiedHarness{HarnessAdapter: harness.NewClaude(cfg.Agents.Claude), Outcomes: []harness.FailureClassification{{Kind: harness.CreditsExhausted}, {Kind: kind, ResetAt: now.Add(time.Hour)}}}, Detection: true}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{"claude": h}})
			if err != nil {
				t.Fatal(err)
			}
			testkit.WaitFor(t, func() bool {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				runs, err := s.Runs(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				for _, run := range runs {
					if run.IssueNumber == 7 && run.State == workflow.NeedsAttention {
						return true
					}
				}
				return false
			})
			if _, err := s.CheckHarness(context.Background(), "claude"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(marker, []byte("go"), 0600); err != nil {
				t.Fatal(err)
			}
			testkit.WaitFor(t, func() bool {
				if _, err := s.Reconcile(context.Background()); err != nil {
					t.Fatal(err)
				}
				runs, err := s.Runs(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				state := workflow.NeedsAttention
				if kind == harness.TemporaryLimit {
					state = workflow.WaitingForHarness
				}
				for _, run := range runs {
					if run.IssueNumber == 8 && run.State == state {
						return true
					}
				}
				return false
			})
			if _, err := s.CheckHarness(context.Background(), "claude"); err == nil {
				t.Fatal("new restriction accepted a second live probe")
			}
			if err := os.WriteFile(proof, []byte("go"), 0600); err != nil {
				t.Fatal(err)
			}
			testkit.WaitFor(t, func() bool {
				if _, err := s.Reconcile(context.Background()); err != nil {
					t.Fatal(err)
				}
				states, _ := s.HarnessAvailability(context.Background())
				for _, v := range states {
					if v.Harness == "claude" {
						return v.Check != nil && (v.Check.Status == "released" || v.Check.Status == "recovered")
					}
				}
				return false
			})
			states, _ := s.HarnessAvailability(context.Background())
			for _, v := range states {
				if v.Harness == "claude" {
					if v.Available || v.ProbeID != "" || (kind == harness.CreditsExhausted && v.Check.Status != "released") || (kind == harness.TemporaryLimit && (v.Reason != "temporary_limit" || !v.ResetAt.Equal(now.Add(time.Hour)))) {
						t.Fatalf("stale proof cleared restriction: %+v", v)
					}
				}
			}
		})
	}
}
