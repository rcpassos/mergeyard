package scheduler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestTierSelectionSurvivesRestartFixAndPublication(t *testing.T) {
	_, runtime, api, _, cfg, r := pairingFlow(t, "claude", "codex", `printf fixed > feature.txt; `+fixedReport)
	cfg.Repositories[0].Implementer.Tiers = map[string]map[string]config.TierSettings{"claude": {"high": {Model: "implement-model", Effort: "medium"}}}
	cfg.Repositories[0].Reviewer.Tiers = map[string]map[string]config.TierSettings{"codex": {"low": {Model: "review-model", Effort: "low"}}}
	cfg.Repositories[0].Implementer.Model, cfg.Repositories[0].Reviewer.Model = "ordinary-implement", "ordinary-review"
	api.issues["owner/repo"][0].Labels = append(api.issues["owner/repo"][0].Labels,
		github.Label{Name: "MERGEYARD:IMPLEMENTER-TIER=HIGH"}, github.Label{Name: "mergeyard:reviewer-tier=low"})
	s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.Plan(context.Background(), "")
	if err != nil || len(plan.Issues) != 1 || plan.Issues[0].RoleSettings == nil || plan.Issues[0].RoleSettings.Implementer.Tier != "high" {
		t.Fatalf("plan tier selection: %+v %v", plan, err)
	}
	run := finish(t, s, workflow.Active, workflow.Review)
	if run.RoleSettings.Implementer.Tier != "high" || run.RoleSettings.Reviewer.Tier != "low" || run.Implementer.Model != "implement-model" {
		t.Fatalf("saved tier selection: %+v", run)
	}
	pr, err := api.GetPullRequest(context.Background(), run.Repository, run.PRNumber)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"MERGEYARD:IMPLEMENTER-TIER=HIGH", "mergeyard:reviewer-tier=low", "implement-model", "review-model"} {
		if !strings.Contains(pr.Body, want) {
			t.Fatalf("PR footer missing %q: %s", want, pr.Body)
		}
	}
	escalation := "<!-- mergeyard:escalation:start -->\nNeeds careful review: migration\n<!-- mergeyard:escalation:end -->\n"
	pr.Body = "Human introduction\n" + strings.Replace(pr.Body, "<!-- mergeyard:generated:end -->", escalation+"<!-- mergeyard:generated:end -->", 1) + "\nHuman checklist"
	root := runtime.Workspace.Root
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	runtime, err = app.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close() })
	cfg.Repositories[0].Implementer = config.Role{Agent: "codex", Model: "changed-implement", Effort: "high", Skills: []string{"changed"}, MaxAttempts: 1}
	cfg.Repositories[0].Reviewer = config.Role{Agent: "claude", Model: "changed-review", Effort: "high", Skills: []string{"changed"}, MaxAttempts: 1}
	api.issues["owner/repo"][0].Labels = []github.Label{{Name: cfg.Repositories[0].Labels.Running}, {Name: "mergeyard:implementer-tier=unknown"}, {Name: "mergeyard:reviewer-tier=high"}}
	s, err = scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := s.Takeover(context.Background(), run.ID)
	command := manualCommand(t, s, run.ID)
	if err != nil || prepared.State != workflow.Manual || command.Executable != cfg.Agents.Claude.Executable || !strings.Contains(strings.Join(command.Args, " "), run.Implementer.SessionID) {
		t.Fatalf("takeover changed tier selection: %+v %v", command, err)
	}
	if _, err := s.Handback(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	done := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if done.Fix.Agent != "claude" || done.Fix.Model != "implement-model" || done.Fix.Effort != "medium" || done.Review.Agent != "codex" || done.Review.Model != "review-model" || done.Review.Effort != "low" || done.RoleSettings.Implementer.Tier != "high" || done.RoleSettings.Reviewer.Tier != "low" {
		t.Fatalf("restart/fix changed tier settings: %+v", done)
	}
	for _, want := range []string{"MERGEYARD:IMPLEMENTER-TIER=HIGH", "mergeyard:reviewer-tier=low", "implement-model", "review-model", escalation} {
		if !strings.Contains(pr.Body, want) {
			t.Fatalf("verification update lost %q: %s", want, pr.Body)
		}
	}
	if !strings.HasPrefix(pr.Body, "Human introduction\n") || !strings.HasSuffix(pr.Body, "\nHuman checklist") || strings.Contains(pr.Body, "changed-review") || strings.Contains(pr.Body, "tier=unknown") {
		t.Fatalf("verification update changed saved selection or human content: %s", pr.Body)
	}
	server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	page := httptest.NewRecorder()
	server.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil))
	for _, want := range []string{"tier high", "MERGEYARD:IMPLEMENTER-TIER=HIGH", "tier low", "mergeyard:reviewer-tier=low", "implement-model", "review-model"} {
		if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), want) {
			t.Fatalf("dashboard missing saved selection %q: %d %s", want, page.Code, page.Body.String())
		}
	}
}

func TestInvalidTierSelectionRefusesClaimOnceAndAllowsCorrection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels []string
	}{
		{"unknown", []string{"mergeyard:implementer-tier=unknown"}},
		{"conflicting", []string{"mergeyard:implementer-tier=low", "mergeyard:implementer-tier=high"}},
		{"undefined", []string{"mergeyard:reviewer-tier=medium"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			labels := tc.labels
			_, runtime, api, _, cfg, r := localFlow(t, successfulScript)
			for _, label := range labels {
				api.issues["owner/repo"][0].Labels = append(api.issues["owner/repo"][0].Labels, github.Label{Name: label})
			}
			deps := scheduler.Dependencies{GitHub: api, Runner: r}
			s, err := scheduler.New(cfg, schedulerResources(runtime), deps)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := s.Plan(context.Background(), "")
			if err != nil || len(plan.Issues) != 1 || plan.Issues[0].Decision != "skipped" || len(api.overrideComments) != 0 {
				t.Fatalf("invalid plan selection: %+v %v", plan, err)
			}
			for range 2 {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			s, err = scheduler.New(cfg, schedulerResources(runtime), deps)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			runs, err := s.Runs(context.Background())
			if err != nil || len(runs) != 0 || len(api.overrideComments) != 1 || !has(api.issues["owner/repo"][0], cfg.Repositories[0].Labels.NeedsAttention) {
				t.Fatalf("invalid selection claimed or replayed: %+v %v comments=%+v", runs, err, api.overrideComments)
			}
			for _, label := range labels {
				if !strings.Contains(api.overrideComments[7], label) {
					t.Fatalf("diagnostic missing %q: %s", label, api.overrideComments[7])
				}
			}
			api.issues["owner/repo"][0].Labels = []github.Label{{Name: cfg.Repositories[0].Labels.Ready}}
			claimed := finish(t, s, workflow.Active, workflow.Review)
			if claimed.RoleSettings.Implementer.Tier != "" || claimed.RoleSettings.Reviewer.Tier != "" || len(api.overrideComments) != 1 {
				t.Fatalf("corrected selection: %+v", claimed.RoleSettings)
			}
		})
	}
}
