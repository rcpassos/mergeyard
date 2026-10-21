package web

import (
	"fmt"
	"html/template"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/doctor"
	"github.com/rcpassos/mergeyard/internal/escalation"
)

// settingsGroup is one read-only table on Settings. The dashboard builds the
// effective configuration into groups in the order the owner reads them:
// global settings, agents, global roles, then each repository and its roles.
type settingsGroup struct {
	Title string
	Rows  []settingsRow
}

type settingsRow struct{ Key, Value string }

// doctorStyle is how one check result reads on Settings: its place in the
// table, its status badge, and any qualifier beside the status.
type doctorStyle struct {
	Rank                  int
	Status, Variant, Kind string
}

var doctorStyles = map[doctor.Severity]doctorStyle{
	doctor.Error:        {Rank: 0, Status: "Fail", Variant: "badge-error"},
	doctor.Warning:      {Rank: 1, Status: "Warn", Variant: "badge-warning"},
	doctor.Unverifiable: {Rank: 2, Status: "Warn", Variant: "badge-warning", Kind: "Unverifiable"},
	doctor.Pass:         {Rank: 3, Status: "Pass", Variant: "badge-success"},
}

func doctorStyleFor(severity doctor.Severity) doctorStyle {
	if style, ok := doctorStyles[severity]; ok {
		return style
	}
	return doctorStyles[doctor.Warning]
}

// newDoctorReport orders findings by severity and keeps the checks that
// passed after them. The report's slices are cloned so a later doctor run
// cannot change the page being rendered.
func newDoctorReport(report doctor.Report) *doctor.Report {
	report.Findings = slices.Clone(report.Findings)
	report.Passes = slices.Clone(report.Passes)
	slices.SortStableFunc(report.Findings, func(a, b doctor.Finding) int {
		return doctorStyleFor(a.Severity).Rank - doctorStyleFor(b.Severity).Rank
	})
	return &report
}

// doctorBadge renders a check's pass, warn or fail status. The text carries
// the status so color is never the only signal.
func doctorBadge(severity doctor.Severity) template.HTML {
	style := doctorStyleFor(severity)
	return template.HTML(`<span class="badge ` + template.HTMLEscapeString(style.Variant) + `">` + template.HTMLEscapeString(style.Status) + `</span>`)
}

// doctorKind keeps an unverifiable check distinct from one that ran and
// warned, while both share the warning status.
func doctorKind(severity doctor.Severity) string {
	return doctorStyleFor(severity).Kind
}

// settingsGroups reads only the effective configuration, the same source as
// the raw YAML disclosure, so the tables expose no value the page did not
// already show. The workspace runtime secret is not part of that configuration.
func settingsGroups(cfg config.Config) []settingsGroup {
	groups := []settingsGroup{
		{Title: "Global settings", Rows: globalSettingsRows(cfg)},
		{Title: "Agents", Rows: agentSettingsRows(cfg.Agents)},
		{Title: "Global implementer", Rows: roleSettingsRows(cfg.Implementer)},
		{Title: "Global reviewer", Rows: roleSettingsRows(cfg.Reviewer)},
	}
	for _, repo := range cfg.Repositories {
		groups = append(groups,
			settingsGroup{Title: repo.Repo, Rows: repositorySettingsRows(repo)},
			settingsGroup{Title: repo.Repo + " · implementer", Rows: roleSettingsRows(repo.Implementer)},
			settingsGroup{Title: repo.Repo + " · reviewer", Rows: roleSettingsRows(repo.Reviewer)},
		)
	}
	return groups
}

func globalSettingsRows(cfg config.Config) []settingsRow {
	rows := []settingsRow{
		{"Config version", fmt.Sprint(cfg.Version)},
		{"Dashboard port", fmt.Sprint(cfg.Port)},
		{"Open browser", yesNo(cfg.OpenBrowser)},
		{"Poll interval", durationText(cfg.PollInterval)},
		{"CI timeout", durationText(cfg.CITimeout)},
		{"Concurrency", fmt.Sprint(cfg.Concurrency)},
		{"Agent ports", fmt.Sprintf("base %d, %d ports per run", cfg.AgentPorts.Base, cfg.AgentPorts.Size)},
	}
	rows = append(rows, sharedSettingsRows(cfg.Labels, cfg.TrustedLabelers, cfg.Escalation)...)
	return append(rows,
		settingsRow{"Max review rounds", fmt.Sprint(cfg.MaxRounds)},
		settingsRow{"PR comments", yesNo(cfg.PRComments)},
		settingsRow{"Usage limit cooldown", durationText(cfg.UsageLimits.Cooldown)},
		settingsRow{"Usage limit max waits", fmt.Sprint(cfg.UsageLimits.MaxWaits)},
	)
}

func agentSettingsRows(agents config.Agents) []settingsRow {
	return []settingsRow{
		{"Claude executable", agents.Claude.Executable},
		{"Claude permission mode", agents.Claude.PermissionMode},
		{"Claude allowed tools", listValue(agents.Claude.AllowedTools, "None")},
		{"Codex executable", agents.Codex.Executable},
		{"Codex sandbox", agents.Codex.Sandbox},
		{"Codex network access", yesNo(agents.Codex.NetworkAccess)},
	}
}

func roleSettingsRows(role config.Role) []settingsRow {
	return []settingsRow{
		{"Agent", role.Agent},
		{"Model", valueOr(role.Model, "Harness default")},
		{"Effort", valueOr(role.Effort, "Harness default")},
		{"Skills", listValue(role.Skills, "None")},
		{"Max attempts", fmt.Sprint(role.MaxAttempts)},
		{"Tiers", tierSettings(role.Tiers)},
	}
}

func repositorySettingsRows(repo config.Repository) []settingsRow {
	rows := []settingsRow{
		{"Enabled", yesNo(repo.Enabled)},
		{"Base branch", valueOr(repo.BaseBranch, "Repository default")},
		{"Concurrency", fmt.Sprint(repo.Concurrency)},
		{"Setup commands", listValue(repo.Setup, "None")},
		{"Checks", listValue(repo.Checks, "None")},
		{"Verify commands", verifyCommands(repo.VerifyCommands)},
		{"Exclude patterns", listValue(repo.Exclude, "None")},
		{"Check retries", fmt.Sprint(repo.CheckRetries)},
		{"Command timeout", durationText(repo.CommandTimeout)},
	}
	return append(rows, sharedSettingsRows(repo.Labels, repo.TrustedLabelers, repo.Escalation)...)
}

// sharedSettingsRows are the labels, trust and escalation values every scope
// resolves, so the global and repository tables show them the same way.
func sharedSettingsRows(labels config.Labels, trusted []string, rules escalation.Rules) []settingsRow {
	return []settingsRow{
		{"Ready label", labels.Ready},
		{"Running label", labels.Running},
		{"Needs attention label", labels.NeedsAttention},
		{"Trusted labelers", listValue(trusted, "None (GitHub write permission)")},
		{"Escalation max changed lines", changedLines(rules.MaxChangedLines)},
		{"Escalation paths", escalationPaths(rules.Paths)},
		{"Escalation new dependencies", yesNo(rules.NewDependencies)},
	}
}

func yesNo(value bool) string {
	if value {
		return "Yes"
	}
	return "No"
}

// valueOr names the fallback an omitted value resolves to, such as the
// repository's default branch or a role's harness defaults.
func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// durationText drops the zero minute and second a whole-hour or whole-minute
// duration carries in Go's standard form ("1h0m0s", "30m0s").
func durationText(value time.Duration) string {
	text := value.String()
	if strings.HasSuffix(text, "m0s") {
		text = strings.TrimSuffix(text, "0s")
	}
	if strings.HasSuffix(text, "h0m") {
		text = strings.TrimSuffix(text, "0m")
	}
	return text
}

func listValue(values []string, empty string) string {
	if len(values) == 0 {
		return empty
	}
	return strings.Join(values, ", ")
}

func verifyCommands(commands []string) string {
	if commands == nil {
		return "Same as checks"
	}
	return listValue(commands, "Disabled")
}

func changedLines(lines int) string {
	if lines <= 0 {
		return "Disabled"
	}
	return fmt.Sprint(lines)
}

func escalationPaths(paths map[string]string) string {
	if len(paths) == 0 {
		return "None"
	}
	patterns := sortedKeys(paths)
	parts := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		parts = append(parts, pattern+" → "+paths[pattern])
	}
	return strings.Join(parts, "; ")
}

func tierSettings(tiers map[string]map[string]config.TierSettings) string {
	if len(tiers) == 0 {
		return "None"
	}
	var parts []string
	for _, agent := range sortedKeys(tiers) {
		for _, tier := range sortedKeys(tiers[agent]) {
			settings := tiers[agent][tier]
			var fields []string
			if settings.Model != "" {
				fields = append(fields, "model "+settings.Model)
			}
			if settings.Effort != "" {
				fields = append(fields, "effort "+settings.Effort)
			}
			detail := "harness defaults"
			if len(fields) > 0 {
				detail = strings.Join(fields, ", ")
			}
			parts = append(parts, fmt.Sprintf("%s %s: %s", agent, tier, detail))
		}
	}
	return strings.Join(parts, "; ")
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
