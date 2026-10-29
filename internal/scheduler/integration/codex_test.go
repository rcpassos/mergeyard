package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestCodexIssueToDraftPR(t *testing.T) {
	t.Parallel()

	s, runtime, api, remote, cfg, r := testkit.TmuxCodexFlow(t, testkit.CodexImplementation)
	run := testkit.Finish(t, s, workflow.Active, workflow.Review)
	if run.PRNumber != 101 || run.ReviewRound != 1 || api.Creations != 1 {
		t.Fatalf("draft endpoint: %+v PRs=%d", run, api.Creations)
	}
	if !api.Prs["mergeyard/issue-7"].Draft {
		t.Fatal("PR must remain draft")
	}
	if testkit.GitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7") != "1" || testkit.GitCommand(t, remote, "show", "mergeyard/issue-7:feature.txt") != "implemented" {
		t.Fatal("expected one pushed implementation")
	}
	restarted, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := restarted.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if api.Creations != 1 {
		t.Fatal("restart duplicated PR")
	}
	if run.Implementer == nil || run.Implementer.Agent != "codex" || run.Implementer.SessionID != testkit.CodexID {
		t.Fatalf("durable identity: %+v", run.Implementer)
	}
}

func TestCodexEarlyIdentityRestartAndStop(t *testing.T) {
	t.Parallel()

	s, runtime, api, _, cfg, r := testkit.TmuxCodexFlow(t, testkit.CodexIdentity+"\n"+`printf keep > pending.txt; sleep 30`)
	run := testkit.WaitCodexIdentity(t, s)
	v := run.Implementer
	if v.Agent != "codex" || v.Attempt != 1 || v.Model != "chosen-model" || v.Effort != "medium" || len(v.Skills) != 2 || !strings.Contains(v.Permissions, "workspace-write") {
		t.Fatalf("settings: %+v", v)
	}
	server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	page := httptest.NewRecorder()
	server.ServeHTTP(page, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil))
	for _, value := range []string{testkit.CodexID, "chosen-model", "medium", "workspace-write", "network true", "implement", "check", v.ProcessSession} {
		if page.Code != 200 || !strings.Contains(page.Body.String(), value) {
			t.Fatalf("dashboard missing %q: %d %s", value, page.Code, page.Body.String())
		}
	}
	ref, err := s.Watch(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	next, err := restarted.Watch(context.Background(), run.ID)
	if err != nil || next != ref {
		t.Fatalf("process duplicated: %+v %v", next, err)
	}
	if err := restarted.Stop(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	runs, err := restarted.Runs(context.Background())
	if err != nil || runs[0].State != workflow.Stopped || runs[0].Implementer.SessionID != testkit.CodexID || runs[0].Implementer.Status != "stopped" {
		t.Fatalf("stop snapshot: %+v %v", runs, err)
	}
	if api.Creations != 0 {
		t.Fatal("stopped attempt created PR")
	}
	status, err := r.SessionStatus(context.Background(), ref)
	if err != nil || status.State == runner.SessionRunning {
		t.Fatalf("process survived stop: %+v %v", status, err)
	}
	history, err := runtime.Events.History(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	discoveries := 0
	for _, event := range history {
		if event.Type == "harness.session_discovered" {
			discoveries++
		}
	}
	if discoveries != 1 {
		t.Fatalf("identity discoveries = %d", discoveries)
	}
}

func TestCodexFailuresStayVisibleAndDoNotPublish(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, Script, Code, diagnostic string }{
		{"invalid", testkit.CodexIdentity + "\n" + strings.ReplaceAll(testkit.CodexResult, `"schema_version":4,"verification":[],"criteria":[],"human_action":null,`, ""), "phase.result_invalid", "requires"},
		{"incomplete", testkit.CodexIdentity + "\n" + strings.ReplaceAll(testkit.CodexResult, `'{"type":"turn.completed"}'`, ""), "phase.result_missing", "native completion"},
		{"startup", `echo 'invalid configuration: effort' >&2; exit 1`, "phase.retries_exhausted", "invalid configuration: effort"},
		{"blocked", testkit.CodexIdentity + "\n" + testkit.BlockedReport(testkit.CodexResult), "phase.blocked", "Added feature"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s, _, api, remote, _, _ := testkit.TmuxCodexFlow(t, tc.Script)
			run := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
			if run.LastErrorCode != tc.Code || !strings.Contains(run.LastErrorMessage, tc.diagnostic) {
				t.Fatalf("diagnostic: %+v", run)
			}
			if run.Implementer == nil || run.Implementer.Status != "failed" || run.Implementer.Attempt != 1 {
				t.Fatalf("attempt: %+v", run.Implementer)
			}
			for range 2 {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if api.Creations != 0 || testkit.GitCommand(t, remote, "branch", "--list", "mergeyard/issue-7") != "" {
				t.Fatal("failed run published code")
			}
		})
	}
}

func TestCodexRetryUsesExactIdentityAndFullInput(t *testing.T) {
	t.Parallel()

	script := testkit.CodexIdentity + "\n" + `case "$*" in
 *'resume -- 01a10c5b-38a9-7330-b833-04e365cb5f37'*)
  case "$*" in *'--model chosen-model -c model_reasoning_effort="medium"'*' $implement $check Read '*) ;; *) echo 'lost retry settings' >&2; exit 1;; esac
  # The retry must get its own complete phase input and strict schema.
  phase_dir=''
  previous=''
  for arg do
   if [ "$previous" = '--output-schema' ]; then phase_dir=${arg%/schema.json}; fi
   previous=$arg
  done
  /usr/bin/grep -q 'Implement this' "$phase_dir/input.md" || exit 1
  /usr/bin/grep -q 'additionalProperties' "$phase_dir/schema.json" || exit 1
 ` + "\n" + testkit.CodexImplementation + "\n" + `;; *) echo 'first attempt failed' >&2; exit 1;; esac`
	s, runtime, api, remote, cfg, r := testkit.TmuxCodexFlow(t, script)
	cfg.Repositories[0].Implementer.MaxAttempts = 2
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.Active, workflow.Review)
	if run.Implementer.Attempt != 2 || run.Implementer.SessionID != testkit.CodexID {
		t.Fatalf("retry snapshot: %+v", run.Implementer)
	}
	if api.Creations != 1 || testkit.GitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7") != "1" {
		t.Fatal("retry duplicated publication")
	}
}

func TestCodexPersistsAttemptBeforeLaunchAndBoundsStartupRetries(t *testing.T) {
	t.Parallel()

	_, runtime, api, _, cfg, r := testkit.TmuxCodexFlow(t, testkit.CodexImplementation)
	cfg.Repositories[0].Implementer.MaxAttempts = 2
	var s *scheduler.Scheduler
	launches := 0
	observed := testkit.ObserveCodexLaunch{Runner: r, Failure: &fault.Error{Code: "phase.launch_failed", Message: "executable unavailable"}, Observe: func(req runner.SessionRequest) {
		launches++
		runs, err := s.Runs(context.Background())
		if err != nil || len(runs) != 1 || runs[0].Implementer == nil {
			t.Fatalf("attempt missing before launch: %+v %v", runs, err)
		}
		v := runs[0].Implementer
		if v.Status != "running" || v.Agent != "codex" || v.ProcessSession == "" || v.Attempt != launches || v.SessionID != "" {
			t.Fatalf("prelaunch identity: %+v", v)
		}
	}}
	var err error
	s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: observed})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
	if launches != 2 || run.Implementer.Attempt != 2 || run.LastErrorCode != "phase.retries_exhausted" || !strings.Contains(run.LastErrorMessage, "executable unavailable") {
		t.Fatalf("startup retry budget: %d %+v", launches, run)
	}
}

func TestCodexIdentityPersistsBeforeNextGitHubPoll(t *testing.T) {
	t.Parallel()

	_, runtime, api, _, cfg, r := testkit.TmuxCodexFlow(t, testkit.CodexIdentity+"\n"+`sleep 30`)
	cfg.PollInterval = time.Hour
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) == 1 && runs[0].Implementer != nil && runs[0].Implementer.SessionID == testkit.CodexID {
			if _, err := s.Watch(context.Background(), runs[0].ID); err != nil {
				t.Fatal(err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("early identity waited for the GitHub polling interval")
}

func TestLegacyCodexRestartDoesNotSendSessionToAnotherHarness(t *testing.T) {
	t.Parallel()

	_, runtime, api, _, cfg, r := testkit.TmuxCodexFlow(t, testkit.CodexIdentity+"\n"+`echo temporary-failure >&2; exit 1`)
	cfg.Repositories[0].Implementer.MaxAttempts = 2
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) == 1 && runs[0].Implementer != nil && runs[0].Implementer.Status == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first attempt did not fail")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Simulate a run claimed by the old binary, without a fixed role selection.
	if _, err := runtime.DB.Exec("UPDATE runs SET role_settings_json=NULL"); err != nil {
		t.Fatal(err)
	}
	cfg.Repositories[0].Implementer.Agent = "claude"
	restarted, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, restarted, workflow.NeedsAttention, workflow.Implement)
	if run.LastErrorCode != "harness.session_agent_changed" || run.Implementer.Agent != "codex" || run.Implementer.SessionID != testkit.CodexID || run.Implementer.Attempt != 1 || api.Creations != 0 {
		t.Fatalf("cross-harness resume: %+v, PRs=%d", run, api.Creations)
	}
}

func TestCodexPermissionDisplayUsesEffectiveNetworkAccess(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, sandbox string
		network       bool
		permissions   string
	}{
		{"workspace network off", "workspace-write", false, "workspace-write · network false · approvals never"},
		{"workspace network on", "workspace-write", true, "workspace-write · network true · approvals never"},
		{"full access network off requested", "danger-full-access", false, "danger-full-access · network true · approvals never"},
		{"full access network on requested", "danger-full-access", true, "danger-full-access · network true · approvals never"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, runtime, api, _, cfg, r := testkit.TmuxCodexFlow(t, testkit.CodexImplementation)
			cfg.Agents.Codex.Sandbox, cfg.Agents.Codex.NetworkAccess = tc.sandbox, tc.network
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			run := testkit.Finish(t, s, workflow.Active, workflow.Review)
			if run.Implementer.Permissions != tc.permissions {
				t.Fatalf("effective permissions = %q, want %q", run.Implementer.Permissions, tc.permissions)
			}
			// The displayed attempt permissions must survive configuration changes.
			cfg.Agents.Codex.Sandbox, cfg.Agents.Codex.NetworkAccess = "workspace-write", false
			restarted, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: restarted, Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"/api/status", "/runs/" + run.ID} {
				response := httptest.NewRecorder()
				request := testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331"+path, nil)
				testkit.SignAPIRequest(t, runtime.Workspace.Root, request)
				server.ServeHTTP(response, request)
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), tc.permissions) {
					t.Fatalf("%s missing effective permissions %q: %d %s", path, tc.permissions, response.Code, response.Body.String())
				}
			}
		})
	}
}

func TestLegacyCodexFixRequestsPreserveHarnessOwnership(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, agent, Code string }{
		{"changed harness before fix", "claude", "harness.session_agent_changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s, runtime, api, _, cfg, r := testkit.TmuxCodexFlow(t, testkit.CodexImplementation)
			if err := os.WriteFile(cfg.Agents.Claude.Executable, []byte("#!/bin/sh\n"+testkit.LoopScript(`printf fixed > feature.txt; `+testkit.FixedReport)+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			run := testkit.Finish(t, s, workflow.Active, workflow.Fix)
			// Legacy sessions must still reject a configuration switch of harness.
			if _, err := runtime.DB.Exec("UPDATE runs SET role_settings_json=NULL WHERE id=?", run.ID); err != nil {
				t.Fatal(err)
			}
			cfg.Repositories[0].Implementer.Agent = tc.agent
			restarted, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			run = testkit.Finish(t, restarted, workflow.NeedsAttention, workflow.Fix)
			if run.LastErrorCode != tc.Code || run.Fix != nil || run.Implementer.SessionID != testkit.CodexID || run.Implementer.Agent != "codex" || api.Creations != 1 {
				t.Fatalf("fix changed harness or launched an unsupported attempt: %+v PRs=%d", run, api.Creations)
			}
		})
	}
}
