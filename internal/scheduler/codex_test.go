package scheduler_test

import (
	"context"
	"github.com/rcpassos/mergeyard/internal/web"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

const codexID = "01a10c5b-38a9-7330-b833-04e365cb5f37"
const codexIdentity = `printf '%s\n' '{"type":"thread.started","thread_id":"01a10c5b-38a9-7330-b833-04e365cb5f37"}'`
const codexResult = `last_message=''
previous=''
for arg do
 if [ "$previous" = '-o' ]; then last_message=$arg; fi
 previous=$arg
done
printf '%s\n' '{"schema_version":1,"status":"success","summary":"Added feature"}' > "$last_message"
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"schema_version\":1,\"status\":\"success\",\"summary\":\"Added feature\"}"}}' '{"type":"turn.completed"}'`
const codexImplementation = codexIdentity + "\n" + `printf '%s\n' implemented > feature.txt` + "\n" + codexResult

func codexFlow(t *testing.T, script string) (*scheduler.Scheduler, *app.Runtime, *fakeGitHub, string, config.Config, runner.Runner) {
	t.Helper()
	_, runtime, api, remote, cfg, r := localFlow(t, successfulScript)
	executable := filepath.Join(t.TempDir(), "fake-codex")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Agents.Codex.Executable = executable
	cfg.Repositories[0].Implementer = config.Role{Agent: "codex", Model: "chosen-model", Effort: "medium", Skills: []string{"implement", "check"}, MaxAttempts: 1}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	return s, runtime, api, remote, cfg, r
}

func TestCodexIssueToDraftPR(t *testing.T) {
	s, runtime, api, remote, cfg, r := codexFlow(t, codexImplementation)
	run := finish(t, s, workflow.Active, workflow.Review)
	if run.PRNumber != 101 || run.ReviewRound != 1 || api.creations != 1 {
		t.Fatalf("draft endpoint: %+v PRs=%d", run, api.creations)
	}
	if !api.prs["mergeyard/issue-7"].Draft {
		t.Fatal("PR must remain draft")
	}
	if gitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7") != "1" || gitCommand(t, remote, "show", "mergeyard/issue-7:feature.txt") != "implemented" {
		t.Fatal("expected one pushed implementation")
	}
	restarted, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := restarted.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if api.creations != 1 {
		t.Fatal("restart duplicated PR")
	}
	if run.Implementer == nil || run.Implementer.Agent != "codex" || run.Implementer.SessionID != codexID {
		t.Fatalf("durable identity: %+v", run.Implementer)
	}
}

func waitCodexIdentity(t *testing.T, s *scheduler.Scheduler) workflow.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) == 1 && runs[0].Implementer != nil && runs[0].Implementer.SessionID == codexID {
			return runs[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no early Codex identity in public run snapshot")
	return workflow.Run{}
}

func TestCodexEarlyIdentityRestartAndStop(t *testing.T) {
	s, runtime, api, _, cfg, r := codexFlow(t, codexIdentity+"\n"+`printf keep > pending.txt; sleep 30`)
	run := waitCodexIdentity(t, s)
	v := run.Implementer
	if v.Agent != "codex" || v.Attempt != 1 || v.Model != "chosen-model" || v.Effort != "medium" || len(v.Skills) != 2 || !strings.Contains(v.Permissions, "workspace-write") {
		t.Fatalf("settings: %+v", v)
	}
	server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	page := httptest.NewRecorder()
	server.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil))
	for _, value := range []string{codexID, "chosen-model", "medium", "workspace-write", "network true", "implement", "check", v.ProcessSession} {
		if page.Code != 200 || !strings.Contains(page.Body.String(), value) {
			t.Fatalf("dashboard missing %q: %d %s", value, page.Code, page.Body.String())
		}
	}
	ref, err := s.Watch(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
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
	if err != nil || runs[0].State != workflow.Stopped || runs[0].Implementer.SessionID != codexID || runs[0].Implementer.Status != "stopped" {
		t.Fatalf("stop snapshot: %+v %v", runs, err)
	}
	if api.creations != 0 {
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
	for _, tc := range []struct{ name, script, code, diagnostic string }{
		{"invalid", codexIdentity + "\n" + strings.ReplaceAll(codexResult, `"schema_version":1,`, ""), "phase.result_invalid", "requires"},
		{"incomplete", codexIdentity + "\n" + strings.ReplaceAll(codexResult, `'{"type":"turn.completed"}'`, ""), "phase.result_missing", "native completion"},
		{"startup", `echo 'invalid configuration: effort' >&2; exit 1`, "phase.retries_exhausted", "invalid configuration: effort"},
		{"blocked", codexIdentity + "\n" + strings.ReplaceAll(codexResult, `"success"`, `"blocked"`), "phase.blocked", "Added feature"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, api, remote, _, _ := codexFlow(t, tc.script)
			run := finish(t, s, workflow.NeedsAttention, workflow.Implement)
			if run.LastErrorCode != tc.code || !strings.Contains(run.LastErrorMessage, tc.diagnostic) {
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
			if api.creations != 0 || gitCommand(t, remote, "branch", "--list", "mergeyard/issue-7") != "" {
				t.Fatal("failed run published code")
			}
		})
	}
}

func TestCodexRetryUsesExactIdentityAndFullInput(t *testing.T) {
	script := codexIdentity + "\n" + `case "$*" in
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
 ` + "\n" + codexImplementation + "\n" + `;; *) echo 'first attempt failed' >&2; exit 1;; esac`
	s, runtime, api, remote, cfg, r := codexFlow(t, script)
	cfg.Repositories[0].Implementer.MaxAttempts = 2
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.Active, workflow.Review)
	if run.Implementer.Attempt != 2 || run.Implementer.SessionID != codexID {
		t.Fatalf("retry snapshot: %+v", run.Implementer)
	}
	if api.creations != 1 || gitCommand(t, remote, "rev-list", "--count", "main..mergeyard/issue-7") != "1" {
		t.Fatal("retry duplicated publication")
	}
}

func TestCodexMissingResumeNeverStartsFreshSession(t *testing.T) {
	script := codexIdentity + "\n" + `case "$*" in
 *'resume -- '*) echo 'Error: thread/resume: thread/resume failed: no rollout found for thread id 01a10c5b-38a9-7330-b833-04e365cb5f37 (code -32600)' >&2; exit 1;;
 *) echo first-failure >&2; exit 1;; esac`
	_, runtime, api, _, cfg, r := codexFlow(t, script)
	cfg.Repositories[0].Implementer.MaxAttempts = 3
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.NeedsAttention, workflow.Implement)
	if run.LastErrorCode != "harness.session_resume_failed" || run.Implementer.Attempt != 2 || run.Implementer.SessionID != codexID {
		t.Fatalf("missing session: %+v", run)
	}
	for range 3 {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	runs, _ := s.Runs(context.Background())
	if runs[0].Implementer.Attempt != 2 || api.creations != 0 {
		t.Fatal("missing session silently recovered")
	}
}

type observeCodexLaunch struct {
	runner.Runner
	observe func(runner.SessionRequest)
	failure error
}

func (r observeCodexLaunch) StartSession(ctx context.Context, req runner.SessionRequest) (runner.SessionRef, error) {
	r.observe(req)
	if r.failure != nil {
		return runner.SessionRef{}, r.failure
	}
	return r.Runner.StartSession(ctx, req)
}

func TestCodexPersistsAttemptBeforeLaunchAndBoundsStartupRetries(t *testing.T) {
	_, runtime, api, _, cfg, r := codexFlow(t, codexImplementation)
	cfg.Repositories[0].Implementer.MaxAttempts = 2
	var s *scheduler.Scheduler
	launches := 0
	observed := observeCodexLaunch{Runner: r, failure: &fault.Error{Code: "phase.launch_failed", Message: "executable unavailable"}, observe: func(req runner.SessionRequest) {
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
	s, err = scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: observed})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.NeedsAttention, workflow.Implement)
	if launches != 2 || run.Implementer.Attempt != 2 || run.LastErrorCode != "phase.retries_exhausted" || !strings.Contains(run.LastErrorMessage, "executable unavailable") {
		t.Fatalf("startup retry budget: %d %+v", launches, run)
	}
}

func TestCodexIdentityPersistsBeforeNextGitHubPoll(t *testing.T) {
	_, runtime, api, _, cfg, r := codexFlow(t, codexIdentity+"\n"+`sleep 30`)
	cfg.PollInterval = time.Hour
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
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
		if len(runs) == 1 && runs[0].Implementer != nil && runs[0].Implementer.SessionID == codexID {
			if _, err := s.Watch(context.Background(), runs[0].ID); err != nil {
				t.Fatal(err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("early identity waited for the GitHub polling interval")
}

func TestCodexRestartDoesNotSendSessionToAnotherHarness(t *testing.T) {
	_, runtime, api, _, cfg, r := codexFlow(t, codexIdentity+"\n"+`echo temporary-failure >&2; exit 1`)
	cfg.Repositories[0].Implementer.MaxAttempts = 2
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
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
	cfg.Repositories[0].Implementer.Agent = "claude"
	restarted, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, restarted, workflow.NeedsAttention, workflow.Implement)
	if run.LastErrorCode != "harness.session_agent_changed" || run.Implementer.Agent != "codex" || run.Implementer.SessionID != codexID || run.Implementer.Attempt != 1 || api.creations != 0 {
		t.Fatalf("cross-harness resume: %+v, PRs=%d", run, api.creations)
	}
}

func TestCodexPermissionDisplayUsesEffectiveNetworkAccess(t *testing.T) {
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
			_, runtime, api, _, cfg, r := codexFlow(t, codexImplementation)
			cfg.Agents.Codex.Sandbox, cfg.Agents.Codex.NetworkAccess = tc.sandbox, tc.network
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			run := finish(t, s, workflow.Active, workflow.Review)
			if run.Implementer.Permissions != tc.permissions {
				t.Fatalf("effective permissions = %q, want %q", run.Implementer.Permissions, tc.permissions)
			}
			// The displayed attempt permissions must survive configuration changes.
			cfg.Agents.Codex.Sandbox, cfg.Agents.Codex.NetworkAccess = "workspace-write", false
			restarted, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: restarted, Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"/api/status", "/runs/" + run.ID} {
				response := httptest.NewRecorder()
				server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331"+path, nil))
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), tc.permissions) {
					t.Fatalf("%s missing effective permissions %q: %d %s", path, tc.permissions, response.Code, response.Body.String())
				}
			}
		})
	}
}

func TestCodexFixRequestsPreserveHarnessOwnership(t *testing.T) {
	for _, tc := range []struct{ name, agent, code string }{
		{"unsupported Codex fix", "codex", "phase.unsupported"},
		{"changed harness before fix", "claude", "harness.session_agent_changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, runtime, api, _, cfg, r := codexFlow(t, codexImplementation)
			if err := os.WriteFile(cfg.Agents.Claude.Executable, []byte("#!/bin/sh\n"+loopScript(`printf fixed > feature.txt; `+fixedReport)+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			run := finish(t, s, workflow.Active, workflow.Fix)
			cfg.Repositories[0].Implementer.Agent = tc.agent
			restarted, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			run = finish(t, restarted, workflow.NeedsAttention, workflow.Fix)
			if run.LastErrorCode != tc.code || run.Fix != nil || run.Implementer.SessionID != codexID || run.Implementer.Agent != "codex" || api.creations != 1 {
				t.Fatalf("fix changed harness or launched an unsupported attempt: %+v PRs=%d", run, api.creations)
			}
		})
	}
}
