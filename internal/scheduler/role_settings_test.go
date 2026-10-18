package scheduler_test

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestHarnessOverrideUsesDefaultsAndConfiguredSkills(t *testing.T) {
	_, runtime, api, _, cfg, r := codexFlow(t, codexImplementation)
	cfg.Repositories[0].Implementer = config.Role{Agent: "claude", Model: "claude-model", Effort: "high", Skills: []string{"implement", "check"}, MaxAttempts: 1}
	api.issues["owner/repo"][0].Labels = append(api.issues["owner/repo"][0].Labels, github.Label{Name: "MERGEYARD:IMPLEMENTER=CODEX"})
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.Active, workflow.Review)
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
	for _, role := range []string{"implementer", "reviewer"} {
		for _, correction := range []string{"label", "configuration"} {
			t.Run(role+"/"+correction, func(t *testing.T) {
				_, runtime, api, _, cfg, r := codexFlow(t, codexImplementation)
				cfg.Repositories[0].Implementer.Agent = "claude"
				selected := &cfg.Repositories[0].Implementer
				if role == "reviewer" {
					selected = &cfg.Repositories[0].Reviewer
				}
				selected.Skills = []string{"plugin:check"}
				label := "mergeyard:" + role + "=codex"
				api.issues["owner/repo"][0].Labels = append(api.issues["owner/repo"][0].Labels, github.Label{Name: label})
				deps := scheduler.Dependencies{GitHub: api, Runner: r}
				s, err := scheduler.New(cfg, schedulerResources(runtime), deps)
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
				issue := api.issues["owner/repo"][0]
				if !has(issue, cfg.Repositories[0].Labels.Ready) || !has(issue, cfg.Repositories[0].Labels.NeedsAttention) || has(issue, cfg.Repositories[0].Labels.Running) || api.creations != 0 || len(api.overrideComments) != 1 {
					t.Fatalf("invalid selection was not left correctable: %+v", issue)
				}
				for _, value := range []string{role, "codex", "config.invalid_skills", "configuration"} {
					if !strings.Contains(api.overrideComments[7], value) {
						t.Fatalf("missing diagnostic %q: %s", value, api.overrideComments[7])
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
				s, err = scheduler.New(cfg, schedulerResources(runtime), deps)
				if err != nil {
					t.Fatal(err)
				}
				run := finish(t, s, workflow.Active, workflow.Review)
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

type harnessWithoutSkillSelection struct{ harness.HarnessAdapter }

func (h harnessWithoutSkillSelection) Capabilities() harness.HarnessCapabilities {
	caps := h.HarnessAdapter.Capabilities()
	caps.SkillSelection = false
	return caps
}

func TestHarnessOverrideRequiresDestinationCapabilities(t *testing.T) {
	_, runtime, api, _, cfg, r := localFlow(t, successfulScript)
	cfg.Repositories[0].Implementer.Skills = []string{"check"}
	api.issues["owner/repo"][0].Labels = append(api.issues["owner/repo"][0].Labels, github.Label{Name: "mergeyard:implementer=codex"})
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{"codex": harnessWithoutSkillSelection{harness.NewCodex(cfg.Agents.Codex)}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(context.Background())
	if err != nil || len(runs) != 0 || !strings.Contains(api.overrideComments[7], "harness.capability_unsupported") {
		t.Fatalf("unsupported destination was claimed: %+v %v, diagnostic=%s", runs, err, api.overrideComments[7])
	}
}

func TestMatchingHarnessOverridesRetainConfiguredSettings(t *testing.T) {
	s, runtime, api, _, cfg, r := pairingFlow(t, "codex", "claude", disputedReport)
	api.issues["owner/repo"][0].Labels = append(api.issues["owner/repo"][0].Labels, github.Label{Name: "mergeyard:implementer=codex"}, github.Label{Name: "mergeyard:reviewer=claude"})
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if run.Implementer.Model != "implement-model" || run.Implementer.Effort != "medium" || run.Review.Model != "review-model" || run.Review.Effort != "low" || run.RoleSettings.Implementer.HarnessSource != "label" || run.RoleSettings.Reviewer.HarnessSource != "label" {
		t.Fatal("same-harness override lost configured model or effort")
	}
}

func TestStoredRoleSettingsSurviveRestartLabelsAndFix(t *testing.T) {
	s, runtime, api, _, cfg, r := pairingFlow(t, "claude", "codex", `printf fixed > feature.txt; `+fixedReport)
	run := finish(t, s, workflow.Active, workflow.Review)
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
	api.issues["owner/repo"][0].Labels = append(api.issues["owner/repo"][0].Labels, github.Label{Name: "mergeyard:implementer=unknown"}, github.Label{Name: "mergeyard:reviewer=claude"})
	s, err = scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := s.Takeover(context.Background(), run.ID)
	command := manualCommand(t, s, run.ID)
	if err != nil || prepared.State != workflow.Manual || command.Executable != cfg.Agents.Claude.Executable || !strings.Contains(strings.Join(command.Args, " "), run.Implementer.SessionID) {
		t.Fatalf("takeover changed implementer: %+v %v", command, err)
	}
	if _, err := s.Handback(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	done := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if done.ID != run.ID || done.Fix.Agent != "claude" || done.Fix.Model != "implement-model" || done.Fix.Effort != "medium" || strings.Join(done.Fix.Skills, ",") != "implement" || done.Review.Agent != "codex" || done.Review.Model != "review-model" || done.Review.Effort != "low" || strings.Join(done.Review.Skills, ",") != "review" {
		t.Fatalf("changed fixed role settings: fix=%+v review=%+v", done.Fix, done.Review)
	}
}

func TestHarnessOverridePersistsThroughCIFix(t *testing.T) {
	_, runtime, base, _, cfg, r := pairingFlow(t, "claude", "claude", disputedReport)
	base.issues["owner/repo"][0].Labels = append(base.issues["owner/repo"][0].Labels, github.Label{Name: "mergeyard:implementer=codex"})
	claudeScript := ciFixScript(`printf repaired > feature.txt; ` + ciFixedReport)
	codexScript := nativeCodexScript(claudeScript)
	codexScript = strings.Replace(codexScript, `case "$*" in *"--model $expected_model "*) ;; *) exit 15;; esac`, `case "$*" in *'--model '*) exit 15;; esac`, 1)
	codexScript = strings.Replace(codexScript, `case "$*" in *"model_reasoning_effort=\"$expected_effort\""*) ;; *) exit 16;; esac`, `case "$*" in *'model_reasoning_effort='*) exit 16;; esac`, 1)
	if err := os.WriteFile(cfg.Agents.Claude.Executable, []byte("#!/bin/sh\n"+claudeScript), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.Agents.Codex.Executable, []byte("#!/bin/sh\n"+codexScript), 0700); err != nil {
		t.Fatal(err)
	}
	api := &ciGitHub{fakeGitHub: base, evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "failure"}}}}
	deps := scheduler.Dependencies{GitHub: ciBranchGitHub{api}, Runner: r}
	s, err := scheduler.New(cfg, schedulerResources(runtime), deps)
	if err != nil {
		t.Fatal(err)
	}
	before := finish(t, s, workflow.WaitingForCI, workflow.Review)
	// Neither repository nor label edits may change a later CI repair.
	cfg.Repositories[0].Implementer.Model = "changed-model"
	cfg.Repositories[0].Implementer.Skills = []string{"changed-skill"}
	base.issues["owner/repo"][0].Labels = []github.Label{{Name: cfg.Repositories[0].Labels.Running}, {Name: "mergeyard:implementer=claude"}}
	s, err = scheduler.New(cfg, schedulerResources(runtime), deps)
	if err != nil {
		t.Fatal(err)
	}
	finish(t, s, workflow.Active, workflow.Fix)
	api.evidence = ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: "success"}}}
	after := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if after.Fix.Agent != "codex" || after.Fix.Model != "" || after.Fix.Effort != "" || strings.Join(after.Fix.Skills, ",") != "implement" || after.Fix.SessionID != before.Implementer.SessionID || after.Review.Agent != "claude" || after.ReviewRound != 2 {
		t.Fatalf("CI repair settings: %+v", after.Fix)
	}
}

func TestInvalidHarnessOverridesRequireLabelCorrection(t *testing.T) {
	for i, labels := range [][]string{
		{"mergeyard:implementer=gemini"},
		{"mergeyard:reviewer=claude", "MERGEYARD:REVIEWER=CODEX"},
		{"mergeyard:implementer=", "mergeyard:reviewer=unknown"},
	} {
		t.Run([]string{"unknown", "conflict", "both-invalid"}[i], func(t *testing.T) {
			_, runtime, api, _, cfg, r := localFlow(t, successfulScript)
			for _, label := range labels {
				api.issues["owner/repo"][0].Labels = append(api.issues["owner/repo"][0].Labels, github.Label{Name: label})
			}
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			for range 3 {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			runs, err := s.Runs(context.Background())
			if err != nil || len(runs) != 0 || !has(api.issues["owner/repo"][0], cfg.Repositories[0].Labels.NeedsAttention) || len(api.overrideComments) != 1 {
				t.Fatalf("invalid override claimed or unreported: %+v %v", runs, err)
			}
			comment := api.overrideComments[7]
			for _, label := range labels {
				if !strings.Contains(comment, label) {
					t.Fatalf("comment missing %q: %s", label, comment)
				}
			}
			for _, value := range []string{"claude", "codex"} {
				if !strings.Contains(comment, value) {
					t.Fatal("comment missing valid values")
				}
			}
			// Corrections alone do not unblock the issue until attention is removed.
			api.issues["owner/repo"][0].Labels = []github.Label{{Name: cfg.Repositories[0].Labels.Ready}, {Name: cfg.Repositories[0].Labels.NeedsAttention}, {Name: "mergeyard:implementer=claude"}, {Name: "MERGEYARD:IMPLEMENTER=CLAUDE"}}
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			runs, _ = s.Runs(context.Background())
			if len(runs) != 0 {
				t.Fatal("correction bypassed attention label")
			}
			if err := api.RemoveLabel(context.Background(), "owner/repo", 7, cfg.Repositories[0].Labels.NeedsAttention); err != nil {
				t.Fatal(err)
			}
			run := finish(t, s, workflow.Active, workflow.Review)
			if run.RoleSettings.Implementer.HarnessSource != "label" || len(api.overrideComments) != 1 {
				t.Fatal("corrected issue failed to claim once")
			}
		})
	}
}

func TestInvalidHarnessOverrideDeliverySurvivesRestart(t *testing.T) {
	_, runtime, api, _, cfg, r := localFlow(t, successfulScript)
	api.issues["owner/repo"][0].Labels = append(api.issues["owner/repo"][0].Labels, github.Label{Name: "mergeyard:implementer=unknown"})
	api.mutate = func(action, _ string, _ int, label string) error {
		if action == "add" && label == cfg.Repositories[0].Labels.NeedsAttention {
			return errors.New("ambiguous label delivery")
		}
		return nil
	}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background()); err == nil {
		t.Fatal("expected interrupted label delivery")
	}
	api.mutate = nil
	s, err = scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := s.Runs(context.Background())
	if err != nil || len(runs) != 0 || len(api.overrideComments) != 1 || !has(api.issues["owner/repo"][0], cfg.Repositories[0].Labels.NeedsAttention) {
		t.Fatal("refusal not recovered")
	}
}

func TestCorrectedHarnessOverridesCancelAmbiguousRefusalDelivery(t *testing.T) {
	_, runtime, api, _, cfg, r := localFlow(t, successfulScript)
	api.issues["owner/repo"][0].Labels = append(api.issues["owner/repo"][0].Labels, github.Label{Name: "mergeyard:implementer=unknown"})
	api.mutate = func(action, _ string, _ int, label string) error {
		if action == "add" && label == cfg.Repositories[0].Labels.NeedsAttention {
			// GitHub committed the label, but the caller never received success.
			api.issues["owner/repo"][0].Labels = append(api.issues["owner/repo"][0].Labels, github.Label{Name: label})
			return errors.New("label response lost")
		}
		return nil
	}
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background()); err == nil {
		t.Fatal("expected ambiguous delivery")
	}
	// Follow the comment while the daemon is offline: fix labels, remove attention.
	api.mutate = nil
	api.issues["owner/repo"][0].Labels = []github.Label{{Name: cfg.Repositories[0].Labels.Ready}, {Name: "mergeyard:implementer=claude"}}
	s, err = scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(context.Background())
	if err != nil || len(runs) != 1 || has(api.issues["owner/repo"][0], cfg.Repositories[0].Labels.NeedsAttention) || len(api.overrideComments) != 1 {
		t.Fatalf("stale refusal blocked corrected issue: runs=%d err=%v labels=%+v", len(runs), err, api.issues["owner/repo"][0].Labels)
	}
}

func TestLegacyRunContinuesReadingRoleConfiguration(t *testing.T) {
	_, runtime, api, _, cfg, r := localFlow(t, successfulScript)
	_, err := runtime.Workflow.Transition(context.Background(), "legacy-run", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Repositories[0].Implementer.Model = "new-legacy-model"
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.Active, workflow.Review)
	if run.ID != "legacy-run" || run.RoleSettings != nil || run.Implementer.Model != "new-legacy-model" {
		t.Fatalf("legacy settings: %+v", run)
	}
}

func TestHarnessOverrideDefaultSettingsReachFixAndReview(t *testing.T) {
	for _, role := range []string{"implementer", "reviewer"} {
		t.Run(role, func(t *testing.T) {
			_, runtime, api, _, cfg, r := pairingFlow(t, "claude", "claude", `printf fixed > feature.txt; `+fixedReport)
			api.issues["owner/repo"][0].Labels = append(api.issues["owner/repo"][0].Labels, github.Label{Name: "mergeyard:" + role + "=codex"})
			script := nativeCodexScript(loopScript(`printf fixed > feature.txt; ` + fixedReport))
			script = strings.Replace(script, `case "$*" in *"--model $expected_model "*) ;; *) exit 15;; esac`, `case "$*" in *'--model '*) exit 15;; esac`, 1)
			script = strings.Replace(script, `case "$*" in *"model_reasoning_effort=\"$expected_effort\""*) ;; *) exit 16;; esac`, `case "$*" in *'model_reasoning_effort='*) exit 16;; esac`, 1)
			if err := os.WriteFile(cfg.Agents.Codex.Executable, []byte("#!/bin/sh\n"+script), 0700); err != nil {
				t.Fatal(err)
			}
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
			if err != nil {
				t.Fatal(err)
			}
			run := finish(t, s, workflow.WaitingForCI, workflow.Review)
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
	for _, limited := range []string{"claude", "codex"} {
		for _, kind := range []harness.FailureKind{harness.TemporaryLimit, harness.CreditsExhausted} {
			t.Run(limited+"/"+string(kind), func(t *testing.T) {
				other := "codex"
				if limited == "codex" {
					other = "claude"
				}
				_, runtime, api, _, cfg, r := pairingFlow(t, "claude", limited, disputedReport)
				cfg.Repositories[0].Implementer.Tiers = map[string]map[string]config.TierSettings{
					"claude": {"high": {Model: "implement-model", Effort: "medium"}},
					"codex":  {"high": {Model: "implement-model", Effort: "medium"}},
				}
				api.issues["owner/repo"][0].Labels = append(api.issues["owner/repo"][0].Labels, github.Label{Name: "mergeyard:implementer-tier=high"})
				cfg.Concurrency, cfg.Repositories[0].Concurrency = 3, 3
				api.issues["owner/repo"][0].Labels = append(api.issues["owner/repo"][0].Labels, github.Label{Name: "mergeyard:implementer=" + limited})
				script := `exit 1`
				adapter := harness.HarnessAdapter(harness.NewClaude(cfg.Agents.Claude))
				executable := cfg.Agents.Claude.Executable
				if limited == "codex" {
					adapter = harness.NewCodex(cfg.Agents.Codex)
					executable = cfg.Agents.Codex.Executable
					script = codexIdentity + "\n" + script
				}
				if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+script), 0700); err != nil {
					t.Fatal(err)
				}
				now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
				fake := &creditHarness{classifiedHarness: &classifiedHarness{HarnessAdapter: adapter, outcome: harness.FailureClassification{Kind: kind, ResetAt: now.Add(time.Hour), Source: "fixture"}}, detection: true}
				deps := scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{limited: fake}}
				s, err := scheduler.New(cfg, schedulerResources(runtime), deps)
				if err != nil {
					t.Fatal(err)
				}
				firstState := workflow.WaitingForHarness
				if kind == harness.CreditsExhausted {
					firstState = workflow.NeedsAttention
				}
				finish(t, s, firstState, workflow.Implement)
				skipped, available := ready(8), ready(9)
				if limited == "codex" {
					skipped.Labels = append(skipped.Labels, github.Label{Name: "mergeyard:implementer=" + limited})
				}
				available.Labels = append(available.Labels, github.Label{Name: "mergeyard:implementer=" + other})
				available.Labels = append(available.Labels, github.Label{Name: "mergeyard:implementer-tier=high"})
				api.issues["owner/repo"] = append(api.issues["owner/repo"], skipped, available)
				if limited == "claude" {
					if err := os.WriteFile(cfg.Agents.Codex.Executable, []byte("#!/bin/sh\n"+codexImplementation), 0700); err != nil {
						t.Fatal(err)
					}
				}
				var claimed workflow.Run
				waitForWithin(t, 10*time.Second, func() bool {
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
	for _, agent := range []string{"claude", "codex"} {
		for _, credits := range []bool{false, true} {
			name := agent + "/usage-limit"
			if credits {
				name = agent + "/credits"
			}
			t.Run(name, func(t *testing.T) {
				_, runtime, api, _, cfg, r := pairingFlow(t, agent, agent, disputedReport)
				cfg.Repositories[0].Implementer.Tiers = map[string]map[string]config.TierSettings{agent: {"low": {Model: "implement-model", Effort: "medium"}}}
				cfg.Repositories[0].Implementer.Model = "ordinary-model"
				api.issues["owner/repo"][0].Labels = append(api.issues["owner/repo"][0].Labels, github.Label{Name: "mergeyard:implementer-tier=low"})
				replacePhaseScript(t, cfg, workflow.Implement, `case "$*" in *implement-0-1*) printf partial > partial.txt; exit 1;; esac
`+successfulScript)
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
				h := &creditHarness{classifiedHarness: &classifiedHarness{HarnessAdapter: adapter, outcome: harness.FailureClassification{Kind: kind, ResetAt: now.Add(time.Hour), Source: "fixture"}}, detection: true}
				deps := scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }, Harnesses: map[string]harness.HarnessAdapter{agent: h}}
				s, err := scheduler.New(cfg, schedulerResources(runtime), deps)
				if err != nil {
					t.Fatal(err)
				}
				waiting := finish(t, s, state, workflow.Implement)
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
				s, err = scheduler.New(cfg, schedulerResources(runtime), deps)
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
				done := finish(t, s, workflow.Active, workflow.Review)
				if done.Implementer.Agent != agent || done.Implementer.Model != "implement-model" || done.Implementer.Effort != "medium" || strings.Join(done.Implementer.Skills, ",") != "implement" || done.Implementer.SessionID != waiting.Implementer.SessionID || done.Implementer.Attempt != 2 || done.RoleSettings.Implementer.Tier != "low" || strings.Join(done.RoleSettings.Implementer.TierLabels, ",") != "mergeyard:implementer-tier=low" {
					t.Fatalf("resume changed settings: %+v", done.Implementer)
				}
			})
		}
	}
}

func TestRunDetailShowsStoredRolesBeforeReviewerStarts(t *testing.T) {
	s, runtime, _, _, cfg, _ := pairingFlow(t, "claude", "codex", disputedReport)
	run := finish(t, s, workflow.Active, workflow.Review)
	cfg.Repositories[0].Reviewer.Model = "new-config-model"
	server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	page := httptest.NewRecorder()
	server.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil))
	for _, value := range []string{"Role settings at claim", "implement-model", "review-model", "source config"} {
		if page.Code != 200 || !strings.Contains(page.Body.String(), value) {
			t.Fatalf("missing %q: %d %s", value, page.Code, page.Body.String())
		}
	}
	if strings.Contains(page.Body.String(), "new-config-model") {
		t.Fatal("run detail used changed configuration")
	}
}
