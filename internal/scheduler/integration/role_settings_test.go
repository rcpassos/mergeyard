package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestHarnessOverrideUsesDefaultsAndConfiguredSkills(t *testing.T) {
	t.Parallel()

	_, runtime, api, _, cfg, r := testkit.TmuxCodexFlow(t, testkit.CodexImplementation)
	cfg.Repositories[0].Implementer = config.Role{Agent: "claude", Model: "claude-model", Effort: "high", Skills: []string{"implement", "check"}, MaxAttempts: 1}
	api.Issues["owner/repo"][0].Labels = append(api.Issues["owner/repo"][0].Labels, github.Label{Name: "MERGEYARD:IMPLEMENTER=CODEX"})
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.Active, workflow.Review)
	if run.Implementer.Agent != "codex" || run.Implementer.Model != "" || run.Implementer.Effort != "" || strings.Join(run.Implementer.Skills, ",") != "implement,check" {
		t.Fatalf("resolved implementer: %+v", run.Implementer)
	}
	history, err := runtime.Events.History(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range history {
		if event.Type != "run.claimed" {
			continue
		}
		var payload struct {
			Run struct {
				RoleSettings config.RunRoleSettings `json:"role_settings"`
			}
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Run.RoleSettings.Implementer.Agent != "codex" || payload.Run.RoleSettings.Implementer.HarnessSource != "label" || payload.Run.RoleSettings.Reviewer.Agent != "claude" || payload.Run.RoleSettings.Reviewer.HarnessSource != "config" {
			t.Fatalf("claim settings: %s", event.Payload)
		}
		return
	}
	t.Fatal("missing claim event")
}

func TestIncompatibleHarnessOverridesRemainCorrectableBeforeClaim(t *testing.T) {
	t.Parallel()

	for _, role := range []string{"implementer", "reviewer"} {
		for _, correction := range []string{"label", "configuration"} {
			t.Run(role+"/"+correction, func(t *testing.T) {
				t.Parallel()

				_, runtime, api, _, cfg, r := testkit.TmuxCodexFlow(t, testkit.CodexImplementation)
				cfg.Repositories[0].Implementer.Agent = "claude"
				selected := &cfg.Repositories[0].Implementer
				if role == "reviewer" {
					selected = &cfg.Repositories[0].Reviewer
				}
				selected.Skills = []string{"plugin:check"}
				label := "mergeyard:" + role + "=codex"
				api.Issues["owner/repo"][0].Labels = append(api.Issues["owner/repo"][0].Labels, github.Label{Name: label})
				deps := scheduler.Dependencies{GitHub: api, Runner: r}
				s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), deps)
				if err != nil {
					t.Fatal(err)
				}
				for range 2 {
					if err := s.Tick(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				runs, err := s.Runs(context.Background())
				if err != nil || len(runs) != 0 {
					t.Fatalf("incompatible settings frozen on run: %+v %v", runs, err)
				}
				issue := api.Issues["owner/repo"][0]
				if !testkit.Has(issue, cfg.Repositories[0].Labels.Ready) || !testkit.Has(issue, cfg.Repositories[0].Labels.NeedsAttention) || testkit.Has(issue, cfg.Repositories[0].Labels.Running) || api.Creations != 0 || len(api.OverrideComments) != 1 {
					t.Fatalf("invalid selection was not left correctable: %+v", issue)
				}
				for _, value := range []string{role, "codex", "config.invalid_skills", "configuration"} {
					if !strings.Contains(api.OverrideComments[7], value) {
						t.Fatalf("missing diagnostic %q: %s", value, api.OverrideComments[7])
					}
				}
				if correction == "label" {
					if err := api.RemoveLabel(context.Background(), "owner/repo", 7, label); err != nil {
						t.Fatal(err)
					}
				} else {
					selected.Skills = []string{"check"}
				}
				if err := api.RemoveLabel(context.Background(), "owner/repo", 7, cfg.Repositories[0].Labels.NeedsAttention); err != nil {
					t.Fatal(err)
				}
				s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), deps)
				if err != nil {
					t.Fatal(err)
				}
				run := testkit.Finish(t, s, workflow.Active, workflow.Review)
				saved := run.RoleSettings.Implementer
				if role == "reviewer" {
					saved = run.RoleSettings.Reviewer
				}
				wantAgent, wantSkill := "claude", "plugin:check"
				if correction == "configuration" {
					wantAgent, wantSkill = "codex", "check"
				}
				if saved.Agent != wantAgent || strings.Join(saved.Skills, ",") != wantSkill {
					t.Fatalf("correction not applied at claim: %+v", saved)
				}
			})
		}
	}

}

func TestMatchingHarnessOverridesRetainConfiguredSettings(t *testing.T) {
	t.Parallel()

	s, runtime, api, _, cfg, r := testkit.TmuxPairingFlow(t, "codex", "claude", testkit.DisputedReport)
	api.Issues["owner/repo"][0].Labels = append(api.Issues["owner/repo"][0].Labels, github.Label{Name: "mergeyard:implementer=codex"}, github.Label{Name: "mergeyard:reviewer=claude"})
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	if run.Implementer.Model != "implement-model" || run.Implementer.Effort != "medium" || run.Review.Model != "review-model" || run.Review.Effort != "low" || run.RoleSettings.Implementer.HarnessSource != "label" || run.RoleSettings.Reviewer.HarnessSource != "label" {
		t.Fatal("same-harness override lost configured model or effort")
	}
}

func TestStoredRoleSettingsSurviveRestartLabelsAndFix(t *testing.T) {

	s, runtime, api, _, cfg, r := testkit.TmuxPairingFlow(t, "claude", "codex", `printf fixed > feature.txt; `+testkit.FixedReport)
	run := testkit.Finish(t, s, workflow.Active, workflow.Review)
	root := runtime.Workspace.Root
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	runtime, err := app.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close() })
	cfg.Repositories[0].Implementer = config.Role{Agent: "codex", Model: "changed-implement", Effort: "high", Skills: []string{"changed"}, MaxAttempts: 1}
	cfg.Repositories[0].Reviewer = config.Role{Agent: "claude", Model: "changed-review", Effort: "high", Skills: []string{"changed"}, MaxAttempts: 1}
	api.Issues["owner/repo"][0].Labels = append(api.Issues["owner/repo"][0].Labels, github.Label{Name: "mergeyard:implementer=unknown"}, github.Label{Name: "mergeyard:reviewer=claude"})
	s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := s.Takeover(context.Background(), run.ID)
	command := testkit.ManualCommand(t, s, run.ID)
	if err != nil || prepared.State != workflow.Manual || command.Executable != cfg.Agents.Claude.Executable || !strings.Contains(strings.Join(command.Args, " "), run.Implementer.SessionID) {
		t.Fatalf("takeover changed implementer: %+v %v", command, err)
	}
	if _, err := s.Handback(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	done := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	if done.ID != run.ID || done.Fix.Agent != "claude" || done.Fix.Model != "implement-model" || done.Fix.Effort != "medium" || strings.Join(done.Fix.Skills, ",") != "implement" || done.Review.Agent != "codex" || done.Review.Model != "review-model" || done.Review.Effort != "low" || strings.Join(done.Review.Skills, ",") != "review" {
		t.Fatalf("changed fixed role settings: fix=%+v review=%+v", done.Fix, done.Review)
	}
}

func TestHarnessOverridePersistsThroughCIFix(t *testing.T) {
	t.Parallel()

	_, runtime, base, _, cfg, r := testkit.TmuxPairingFlow(t, "claude", "claude", testkit.DisputedReport)
	base.Issues["owner/repo"][0].Labels = append(base.Issues["owner/repo"][0].Labels, github.Label{Name: "mergeyard:implementer=codex"})
	claudeScript := testkit.CiFixScript(`printf repaired > feature.txt; ` + testkit.CiFixedReport)
	codexScript := testkit.NativeCodexScript(claudeScript)
	codexScript = strings.Replace(codexScript, `case "$*" in *"--model $expected_model "*) ;; *) exit 15;; esac`, `case "$*" in *'--model '*) exit 15;; esac`, 1)
	codexScript = strings.Replace(codexScript, `case "$*" in *"model_reasoning_effort=\"$expected_effort\""*) ;; *) exit 16;; esac`, `case "$*" in *'model_reasoning_effort='*) exit 16;; esac`, 1)
	if err := os.WriteFile(cfg.Agents.Claude.Executable, []byte("#!/bin/sh\n"+claudeScript), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.Agents.Codex.Executable, []byte("#!/bin/sh\n"+codexScript), 0700); err != nil {
		t.Fatal(err)
	}
	api := &testkit.CiGitHub{FakeGitHub: base, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "failure"}}}}
	deps := scheduler.Dependencies{GitHub: testkit.CiBranchGitHub{CiGitHub: api}, Runner: r}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), deps)
	if err != nil {
		t.Fatal(err)
	}
	before := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	// Neither repository nor label edits may change a later CI repair.
	cfg.Repositories[0].Implementer.Model = "changed-model"
	cfg.Repositories[0].Implementer.Skills = []string{"changed-skill"}
	base.Issues["owner/repo"][0].Labels = []github.Label{{Name: cfg.Repositories[0].Labels.Running}, {Name: "mergeyard:implementer=claude"}}
	s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), deps)
	if err != nil {
		t.Fatal(err)
	}
	testkit.Finish(t, s, workflow.Active, workflow.Fix)
	api.Evidence = ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "success"}}}
	after := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	if after.Fix.Agent != "codex" || after.Fix.Model != "" || after.Fix.Effort != "" || strings.Join(after.Fix.Skills, ",") != "implement" || after.Fix.SessionID != before.Implementer.SessionID || after.Review.Agent != "claude" || after.ReviewRound != 2 {
		t.Fatalf("CI repair settings: %+v", after.Fix)
	}
}

func TestHarnessOverrideDefaultSettingsReachFixAndReview(t *testing.T) {
	t.Parallel()

	for _, role := range []string{"implementer", "reviewer"} {
		t.Run(role, func(t *testing.T) {
			t.Parallel()

			_, runtime, api, _, cfg, r := testkit.TmuxPairingFlow(t, "claude", "claude", `printf fixed > feature.txt; `+testkit.FixedReport)
			api.Issues["owner/repo"][0].Labels = append(api.Issues["owner/repo"][0].Labels, github.Label{Name: "mergeyard:" + role + "=codex"})
			script := testkit.NativeCodexScript(testkit.LoopScript(`printf fixed > feature.txt; ` + testkit.FixedReport))
			script = strings.Replace(script, `case "$*" in *"--model $expected_model "*) ;; *) exit 15;; esac`, `case "$*" in *'--model '*) exit 15;; esac`, 1)
			script = strings.Replace(script, `case "$*" in *"model_reasoning_effort=\"$expected_effort\""*) ;; *) exit 16;; esac`, `case "$*" in *'model_reasoning_effort='*) exit 16;; esac`, 1)
			if err := os.WriteFile(cfg.Agents.Codex.Executable, []byte("#!/bin/sh\n"+script), 0700); err != nil {
				t.Fatal(err)
			}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
			if role == "implementer" {
				if run.Fix.Agent != "codex" || run.Fix.Model != "" || run.Fix.Effort != "" || run.Review.Agent != "claude" {
					t.Fatalf("override fix: %+v", run.Fix)
				}
			} else if run.Review.Agent != "codex" || run.Review.Model != "" || run.Review.Effort != "" || run.Fix.Agent != "claude" {
				t.Fatalf("override review: %+v", run.Review)
			}
		})
	}
}

func TestPerIssueHarnessGateSkipsOnlyRestrictedImplementers(t *testing.T) {
	t.Parallel()

	for _, limited := range []string{"claude", "codex"} {
		for _, kind := range []harness.FailureKind{harness.TemporaryLimit, harness.CreditsExhausted} {
			t.Run(limited+"/"+string(kind), func(t *testing.T) {
				t.Parallel()

				other := "codex"
				if limited == "codex" {
					other = "claude"
				}
				_, runtime, api, _, cfg, r := testkit.TmuxPairingFlow(t, "claude", limited, testkit.DisputedReport)
				cfg.Repositories[0].Implementer.Tiers = map[string]map[string]config.TierSettings{
					"claude": {"high": {Model: "implement-model", Effort: "medium"}},
					"codex":  {"high": {Model: "implement-model", Effort: "medium"}},
				}
				api.Issues["owner/repo"][0].Labels = append(api.Issues["owner/repo"][0].Labels, github.Label{Name: "mergeyard:implementer-tier=high"})
				cfg.Concurrency, cfg.Repositories[0].Concurrency = 3, 3
				api.Issues["owner/repo"][0].Labels = append(api.Issues["owner/repo"][0].Labels, github.Label{Name: "mergeyard:implementer=" + limited})
				script := `exit 1`
				adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
				executable := cfg.Agents.Claude.Executable
				if limited == "codex" {
					adapter = harness.NewCodex(cfg.Agents.Codex)
					executable = cfg.Agents.Codex.Executable
					script = testkit.CodexIdentity + "\n" + script
				}
				if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+script), 0700); err != nil {
					t.Fatal(err)
				}
				now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
				fake := &testkit.CreditHarness{ClassifiedHarness: &testkit.ClassifiedHarness{HarnessAdapter: adapter, Outcome: harness.FailureClassification{Kind: kind, ResetAt: now.Add(time.Hour), Source: "fixture"}}, Detection: true}
				deps := scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{limited: fake}}
				s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), deps)
				if err != nil {
					t.Fatal(err)
				}
				firstState := workflow.WaitingForHarness
				if kind == harness.CreditsExhausted {
					firstState = workflow.NeedsAttention
				}
				testkit.Finish(t, s, firstState, workflow.Implement)
				skipped, available := testkit.Ready(8), testkit.Ready(9)
				if limited == "codex" {
					skipped.Labels = append(skipped.Labels, github.Label{Name: "mergeyard:implementer=" + limited})
				}
				available.Labels = append(available.Labels, github.Label{Name: "mergeyard:implementer=" + other})
				available.Labels = append(available.Labels, github.Label{Name: "mergeyard:implementer-tier=high"})
				api.Issues["owner/repo"] = append(api.Issues["owner/repo"], skipped, available)
				if limited == "claude" {
					if err := os.WriteFile(cfg.Agents.Codex.Executable, []byte("#!/bin/sh\n"+testkit.CodexImplementation), 0700); err != nil {
						t.Fatal(err)
					}
				}
				var claimed workflow.Run
				testkit.WaitForWithin(t, 10*time.Second, func() bool {
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
						if run.IssueNumber == 8 {
							t.Fatal("restricted issue was claimed")
						}
						if run.IssueNumber == 9 {
							claimed = run
						}
					}
					return claimed.State == workflow.WaitingForHarness && claimed.Phase == workflow.Review
				})
				if claimed.Implementer.Agent != other || claimed.Review != nil || claimed.HarnessWait.Harness != limited {
					t.Fatalf("per-issue role gate: %+v", claimed)
				}
			})
		}
	}
}

func TestStoredRoleSettingsSurviveUsageResumeAndCreditRetry(t *testing.T) {
	t.Parallel()

	for _, agent := range []string{"claude", "codex"} {
		for _, credits := range []bool{false, true} {
			name := agent + "/usage-limit"
			if credits {
				name = agent + "/credits"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				_, runtime, api, _, cfg, r := testkit.TmuxPairingFlow(t, agent, agent, testkit.DisputedReport)
				cfg.Repositories[0].Implementer.Tiers = map[string]map[string]config.TierSettings{agent: {"low": {Model: "implement-model", Effort: "medium"}}}
				cfg.Repositories[0].Implementer.Model = "ordinary-model"
				api.Issues["owner/repo"][0].Labels = append(api.Issues["owner/repo"][0].Labels, github.Label{Name: "mergeyard:implementer-tier=low"})
				testkit.ReplacePhaseScript(t, cfg, workflow.Implement, `case "$*" in *implement-0-1*) printf partial > partial.txt; exit 1;; esac
`+testkit.SuccessfulScript)
				adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
				if agent == "codex" {
					adapter = harness.NewCodex(cfg.Agents.Codex)
				}
				now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
				kind := harness.TemporaryLimit
				state := workflow.WaitingForHarness
				if credits {
					kind, state = harness.CreditsExhausted, workflow.NeedsAttention
				}
				h := &testkit.CreditHarness{ClassifiedHarness: &testkit.ClassifiedHarness{HarnessAdapter: adapter, Outcome: harness.FailureClassification{Kind: kind, ResetAt: now.Add(time.Hour), Source: "fixture"}}, Detection: true}
				deps := scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{agent: h}}
				s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), deps)
				if err != nil {
					t.Fatal(err)
				}
				waiting := testkit.Finish(t, s, state, workflow.Implement)
				root := runtime.Workspace.Root
				if err := runtime.Close(); err != nil {
					t.Fatal(err)
				}
				runtime, err = app.Open(context.Background(), root)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { runtime.Close() })
				other := "codex"
				if agent == "codex" {
					other = "claude"
				}
				cfg.Repositories[0].Implementer = config.Role{Agent: other, Model: "changed", Effort: "high", Skills: []string{"changed"}, MaxAttempts: 1}
				s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), deps)
				if err != nil {
					t.Fatal(err)
				}
				now = now.Add(2 * time.Hour)
				if credits {
					selected, err := s.Retry(context.Background(), waiting.ID)
					if err != nil {
						t.Fatal(err)
					}
					if len(selected.CreditProbes) != 1 || selected.CreditProbes[0].Harness != agent {
						t.Fatalf("wrong probe harness: %+v", selected.CreditProbes)
					}
				}
				done := testkit.Finish(t, s, workflow.Active, workflow.Review)
				if done.Implementer.Agent != agent || done.Implementer.Model != "implement-model" || done.Implementer.Effort != "medium" || strings.Join(done.Implementer.Skills, ",") != "implement" || done.Implementer.SessionID != waiting.Implementer.SessionID || done.Implementer.Attempt != 2 || done.RoleSettings.Implementer.Tier != "low" || strings.Join(done.RoleSettings.Implementer.TierLabels, ",") != "mergeyard:implementer-tier=low" {
					t.Fatalf("resume changed settings: %+v", done.Implementer)
				}
			})
		}
	}
}

func TestRunDetailShowsStoredRolesBeforeReviewerStarts(t *testing.T) {
	t.Parallel()

	s, runtime, _, _, cfg, _ := testkit.TmuxPairingFlow(t, "claude", "codex", testkit.DisputedReport)
	run := testkit.Finish(t, s, workflow.Active, workflow.Review)
	cfg.Repositories[0].Reviewer.Model = "new-config-model"
	server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	page := httptest.NewRecorder()
	server.ServeHTTP(page, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil))
	for _, value := range []string{"Role settings at claim", "implement-model", "review-model", "source config"} {
		if page.Code != 200 || !strings.Contains(page.Body.String(), value) {
			t.Fatalf("missing %q: %d %s", value, page.Code, page.Body.String())
		}
	}
	if strings.Contains(page.Body.String(), "new-config-model") {
		t.Fatal("run detail used changed configuration")
	}
}
