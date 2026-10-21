package web

import (
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/doctor"
)

func settingsRowValue(t *testing.T, groups []settingsGroup, title, key string) string {
	t.Helper()
	for _, group := range groups {
		if group.Title != title {
			continue
		}
		for _, row := range group.Rows {
			if row.Key == key {
				return row.Value
			}
		}
		t.Fatalf("group %q has no row %q", title, key)
	}
	t.Fatalf("settings has no group %q", title)
	return ""
}

func TestSettingsGroupsCoverGlobalSettingsRepositoriesAndRoles(t *testing.T) {
	cfg, _, err := config.Parse([]byte(`
version: 1
port: 7444
concurrency: 3
repositories:
  - repo: owner/repo
    base_branch: trunk
    checks: ["go test ./..."]
    implementer:
      agent: codex
      model: implementation
      effort: high
      tiers:
        codex:
          high: {model: implementation-model, effort: high}
    reviewer:
      skills: [review]
  - repo: owner/site
    enabled: false
`))
	if err != nil {
		t.Fatal(err)
	}
	groups := settingsGroups(cfg)
	var titles []string
	for _, group := range groups {
		titles = append(titles, group.Title)
	}
	want := []string{"Global settings", "Agents", "Global implementer", "Global reviewer", "owner/repo", "owner/repo · implementer", "owner/repo · reviewer", "owner/site", "owner/site · implementer", "owner/site · reviewer"}
	if strings.Join(titles, "|") != strings.Join(want, "|") {
		t.Fatalf("group order = %v, want %v", titles, want)
	}
	for _, tc := range []struct{ group, key, want string }{
		{"Global settings", "Dashboard port", "7444"},
		{"Global settings", "Concurrency", "3"},
		{"Global settings", "CI timeout", "1h"},
		{"Global settings", "Usage limit cooldown", "30m"},
		{"Global settings", "Ready label", "ready-for-agent"},
		{"Global settings", "Trusted labelers", "None (GitHub write permission)"},
		{"Agents", "Claude permission mode", "bypassPermissions"},
		{"Global implementer", "Model", "Harness default"},
		{"owner/repo", "Base branch", "trunk"},
		{"owner/repo", "Checks", "go test ./..."},
		{"owner/repo", "Verify commands", "Same as checks"},
		{"owner/repo", "Command timeout", "20m"},
		{"owner/repo · implementer", "Agent", "codex"},
		{"owner/repo · implementer", "Model", "implementation"},
		{"owner/repo · implementer", "Tiers", "codex high: model implementation-model, effort high"},
		{"owner/repo · reviewer", "Model", "Harness default"},
		{"owner/repo · reviewer", "Skills", "review"},
		{"owner/site", "Enabled", "No"},
	} {
		if got := settingsRowValue(t, groups, tc.group, tc.key); got != tc.want {
			t.Errorf("%s / %s = %q, want %q", tc.group, tc.key, got, tc.want)
		}
	}
}

func TestDoctorReportOrdersFindingsAndKeepsPasses(t *testing.T) {
	report := doctor.Report{
		Findings: []doctor.Finding{
			{Severity: doctor.Warning, Code: "harness.instructions_missing", Scope: "owner/repo", Message: "Codex is assigned but the base branch has CLAUDE.md without AGENTS.md"},
			{Severity: doctor.Unverifiable, Code: "github.push_unverifiable", Scope: "owner/site", Message: "GitHub did not report permissions.push"},
			{Severity: doctor.Error, Code: "github.label_missing", Scope: "owner/repo", Message: `required label "agent-running" is missing`},
		},
		Passes: []doctor.Finding{
			{Severity: doctor.Pass, Code: "config.valid", Scope: "configuration", Message: "configuration loaded and validated"},
		},
	}
	view := newDoctorReport(report)
	if len(view.Findings) != 3 || view.Findings[0].Severity != doctor.Error || view.Findings[1].Severity != doctor.Warning || view.Findings[2].Severity != doctor.Unverifiable {
		t.Fatalf("findings order: %+v", view.Findings)
	}
	if view.Findings[0].Message != report.Findings[2].Message || len(view.Passes) != 1 || view.Passes[0].Code != "config.valid" {
		t.Fatalf("doctor report lost results: %+v", view)
	}
	for _, tc := range []struct {
		severity doctor.Severity
		badge    string
		kind     string
	}{
		{doctor.Pass, `<span class="badge badge-success">Pass</span>`, ""},
		{doctor.Warning, `<span class="badge badge-warning">Warn</span>`, ""},
		{doctor.Unverifiable, `<span class="badge badge-warning">Warn</span>`, "Unverifiable"},
		{doctor.Error, `<span class="badge badge-error">Fail</span>`, ""},
	} {
		if got := string(doctorBadge(tc.severity)); got != tc.badge {
			t.Errorf("%s badge = %q, want %q", tc.severity, got, tc.badge)
		}
		if got := doctorKind(tc.severity); got != tc.kind {
			t.Errorf("%s kind = %q, want %q", tc.severity, got, tc.kind)
		}
	}
}

func TestSettingsShowsDiagnosticsNotRunAndDisconnectedConfiguration(t *testing.T) {
	body := renderContent(t, pageData{Page: "settings", Path: "/settings", Title: "Settings & diagnostics", ConfigText: "Runtime configuration is not connected."})
	if !strings.Contains(body, "Runtime configuration is not connected.") || !strings.Contains(body, "Diagnostics have not been run.") {
		t.Fatalf("settings fallback missing: %s", body)
	}
}
