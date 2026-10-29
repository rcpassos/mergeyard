package scheduler_test

import (
	"context"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/doctor"
	"github.com/rcpassos/mergeyard/internal/runtimeauth"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
)

// Settings presents the loaded configuration and the startup doctor report as
// read-only tables. Recovery and handback history belong to each run's page.
func TestSettingsRendersConfigurationTablesAndStructuredDoctorResults(t *testing.T) {
	t.Parallel()

	_, runtime := testkit.Fixture(t, &testkit.FakeGitHub{})
	cfg, _, err := config.Parse([]byte(`
port: 7444
concurrency: 3
repositories:
  - repo: owner/repo
    base_branch: trunk
    concurrency: 2
    setup: ["echo <script>alert(2)</script>"]
    checks: ["go test ./..."]
    implementer:
      agent: codex
      model: implementation
      effort: high
    reviewer:
      model: independent-review
  - repo: owner/site
    enabled: false
`))
	if err != nil {
		t.Fatal(err)
	}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: &testkit.FakeGitHub{}})
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "mergeyard.yaml")
	report := doctor.Report{
		Findings: []doctor.Finding{
			{Severity: doctor.Error, Code: "github.label_missing", Scope: "owner/repo", Message: `required label "agent-running" is missing`},
			{Severity: doctor.Warning, Code: "harness.instructions_missing", Scope: "owner/repo", Message: "Codex is assigned but the base branch has CLAUDE.md without AGENTS.md"},
			{Severity: doctor.Warning, Code: "github.invalid_response", Scope: "owner/site", Message: `cannot parse <script>alert(1)</script>`},
			{Severity: doctor.Unverifiable, Code: "github.push_unverifiable", Scope: "owner/site", Message: "GitHub did not report permissions.push"},
		},
		Passes: []doctor.Finding{
			{Severity: doctor.Pass, Code: "config.valid", Scope: "configuration", Message: "configuration loaded and validated"},
			{Severity: doctor.Pass, Code: "github.push_permission", Scope: "owner/repo", Message: "the authenticated GitHub account has push permission"},
		},
	}
	options := web.DashboardOptions{
		DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg, ConfigPath: configPath,
		Diagnostics: func(context.Context) doctor.Report { return report },
	}
	server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, options)
	if err != nil {
		t.Fatal(err)
	}
	body := testkit.SettingsPage(t, server, "github.label_missing")
	for _, want := range []string{
		"Edit the YAML configuration and restart Mergeyard",
		"Configuration path", configPath, "Workspace", runtime.Workspace.Root,
		"Global settings", "Agents", "Global implementer", "Global reviewer",
		"owner/repo", "owner/repo · implementer", "owner/repo · reviewer", "owner/site",
		"Dashboard port", "7444", "Concurrency", "3", "implementation", "independent-review",
		"Base branch", "trunk", "go test ./...", "&lt;script&gt;alert(2)&lt;/script&gt;",
		"Raw effective YAML", "<details", "concurrency: 3",
		"Doctor results",
		`<span class="badge badge-error">Fail</span>`, "required label",
		`<span class="badge badge-warning">Warn</span>`, "CLAUDE.md without AGENTS.md", "GitHub did not report permissions.push",
		`<span class="badge badge-success">Pass</span>`, "configuration loaded and validated", "the authenticated GitHub account has push permission",
		"Unverifiable", "&lt;script&gt;alert(1)&lt;/script&gt;",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings is missing %q", want)
		}
	}
	if count := strings.Count(body, `<span class="badge badge-warning">Warn</span>`); count != 3 {
		t.Errorf("doctor rendered %d warn rows, want 3", count)
	}
	if count := strings.Count(body, `<span class="badge badge-success">Pass</span>`); count != 2 {
		t.Errorf("doctor rendered %d pass rows, want 2", count)
	}
	for _, raw := range []string{"<script>alert(1)</script>", "<script>alert(2)</script>"} {
		if strings.Contains(body, raw) {
			t.Errorf("settings did not escape %q: %s", raw, body)
		}
	}
	for _, removed := range []string{"Harness recovery history", "Manual handback history"} {
		if strings.Contains(body, removed) {
			t.Errorf("settings still renders %q", removed)
		}
	}
	secret, err := runtimeauth.LoadSecret(runtime.Workspace.Root)
	if err != nil {
		t.Fatal(err)
	}
	// The runtime secret belongs to the CLI API; it must never reach a page,
	// table or the raw configuration disclosure.
	if strings.Contains(body, hex.EncodeToString(secret)) {
		t.Errorf("settings exposed the workspace secret")
	}

	// A report without findings still shows every check that passed.
	cleanOptions := options
	cleanOptions.Diagnostics = func(context.Context) doctor.Report {
		return doctor.Report{Passes: []doctor.Finding{{Severity: doctor.Pass, Code: "config.valid", Scope: "configuration", Message: "configuration loaded and validated"}}}
	}
	cleanServer, err := web.NewDashboard(runtime.Events, runtime.Scheduler, cleanOptions)
	if err != nil {
		t.Fatal(err)
	}
	clean := testkit.SettingsPage(t, cleanServer, "configuration loaded and validated")
	if !strings.Contains(clean, `<span class="badge badge-success">Pass</span>`) {
		t.Errorf("clean settings has no pass row")
	}
}
