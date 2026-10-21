package web

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/acceptance"
	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/escalation"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/maintenance"
	"github.com/rcpassos/mergeyard/internal/review"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

var alertOpening = regexp.MustCompile(`<(\w+)[^>]*\bclass="alert alert-(info|success|warning|error)\b[^"]*"[^>]*>`)

// alertVariant reports the daisyUI alert variant of the element enclosing the
// first occurrence of text, or "" when text sits outside every alert.
func alertVariant(body, text string) string {
	at := strings.Index(body, text)
	if at < 0 {
		return ""
	}
	variant := ""
	for _, match := range alertOpening.FindAllStringSubmatchIndex(body[:at], -1) {
		tag := body[match[2]:match[3]]
		depth := 0
		for _, edge := range regexp.MustCompile(`</?`+tag+`\b`).FindAllString(body[match[1]:at], -1) {
			if strings.HasPrefix(edge, "</") {
				depth--
			} else {
				depth++
			}
			if depth < 0 {
				break
			}
		}
		if depth >= 0 {
			variant = body[match[4]:match[5]]
		}
	}
	return variant
}

func renderContent(t *testing.T, data pageData) string {
	t.Helper()
	set, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	if err := set.ExecuteTemplate(&body, "content", data); err != nil {
		t.Fatal(err)
	}
	return body.String()
}

func assertAlerts(t *testing.T, body string, want map[string]string) {
	t.Helper()
	for text, variant := range want {
		if !strings.Contains(body, text) {
			t.Errorf("%q is not rendered", text)
			continue
		}
		if got := alertVariant(body, text); got != variant {
			t.Errorf("%q renders in alert %q, want %q", text, got, variant)
		}
	}
}

func TestQueueMessagesRenderAsSeverityAlerts(t *testing.T) {
	data := pageData{Page: "dashboard", Path: "/", Title: "Dashboard", Sections: emptySections(), QueueError: "GitHub discovery failed for owner/repo."}
	data.Sections[blockedSection].Issues = []issueView{{Repository: "owner/repo", Issue: github.Issue{Number: 8, Title: "Refused issue"}, RefusalReason: "Untrusted label actor."}}
	data.Sections[attentionSection].Failures = []scheduler.Finding{{Code: "github.rate_limited", Repository: "owner/repo", Message: "GitHub rate limit reached."}}
	data.Sections[runningSection].Runs = []runView{{Run: workflow.Run{ID: "run-ci", Repository: "owner/repo", IssueNumber: 9, State: workflow.WaitingForCI,
		CI: &ci.Snapshot{Warning: "Required check list is empty.", BaseWarning: "Base branch moved during verification."}}}}
	data.Sections[attentionSection].Runs = []runView{
		{Run: workflow.Run{ID: "run-human", Repository: "owner/repo", IssueNumber: 10, State: workflow.NeedsAttention,
			HumanAction: &fault.HumanAction{Decision: "Choose which migration wins."}}},
		{Run: workflow.Run{ID: "run-error", Repository: "owner/repo", IssueNumber: 11, State: workflow.NeedsAttention,
			LastErrorCode: "phase.failed", LastErrorMessage: "Implementer exited with status 2."}},
	}
	data.Sections[draftPRSection].Runs = []runView{{Run: workflow.Run{ID: "run-merge", Repository: "owner/repo", IssueNumber: 12, State: workflow.ReadyToMerge,
		Escalation: &escalation.Snapshot{EvaluationWarning: "Escalation rules could not be evaluated."}}}}

	assertAlerts(t, renderContent(t, data), map[string]string{
		"Discovering ready and blocked issues":     "info",
		"GitHub discovery failed for owner/repo.":  "error",
		"Untrusted label actor.":                   "warning",
		"GitHub rate limit reached.":               "error",
		"Required check list is empty.":            "warning",
		"Base branch moved during verification.":   "warning",
		"Choose which migration wins.":             "error",
		"Implementer exited with status 2.":        "error",
		"Escalation rules could not be evaluated.": "warning",
	})
}

func TestRunDetailMessagesRenderAsSeverityAlerts(t *testing.T) {
	cases := []struct {
		name string
		run  runView
		want map[string]string
	}{
		{"merged", runView{Run: workflow.Run{State: workflow.Completed, Merge: &maintenance.Snapshot{ObservedAt: "2026-10-21T10:00:00Z",
			ReadyRemoved: true, RunningRemoved: true, AttentionRemoved: true, IssueClosed: true, ReviewRestored: true, WorktreeRemoved: true, BranchDeleted: true, ProcessExited: true}}},
			map[string]string{"Merged — cleanup complete": "success"}},
		{"cleanup failed", runView{Run: workflow.Run{State: workflow.Completed, Merge: &maintenance.Snapshot{ObservedAt: "2026-10-21T10:00:00Z", Error: "Could not delete the branch."}}},
			map[string]string{"Merged — cleanup pending": "warning", "Could not delete the branch.": "error"}},
		{"takeover blocked", runView{Run: workflow.Run{State: workflow.NeedsAttention}, TakeoverPrerequisite: "Takeover needs a prepared worktree."},
			map[string]string{"Takeover needs a prepared worktree.": "warning"}},
		{"approved", runView{Run: workflow.Run{State: workflow.ReadyToMerge}}, map[string]string{"Waiting for your merge": "success"}},
		{"review queued", runView{Run: workflow.Run{State: workflow.Active}, ReviewPending: true}, map[string]string{"Independent review will start": "info"}},
		{"harness wait", runView{Run: workflow.Run{State: workflow.WaitingForHarness,
			HarnessWait: &workflow.HarnessWait{Harness: "claude", Reason: "usage_limited", ResetAt: time.Date(2026, 10, 21, 14, 30, 0, 0, time.UTC)}}},
			map[string]string{"Waiting for claude until": "warning"}},
		{"failure", runView{Run: workflow.Run{State: workflow.NeedsAttention, LastErrorCode: "phase.failed", LastErrorMessage: "Implementer exited with status 2.",
			HumanAction: &fault.HumanAction{Decision: "Choose which migration wins."}},
			StoppedPublications: []scheduler.StoppedPublication{{Label: "Review report", Code: "github.forbidden"}}},
			map[string]string{"Implementer exited with status 2.": "error", "Choose which migration wins.": "error", "These publications stopped for good": "error"}},
		{"review outcomes", runView{Run: workflow.Run{State: workflow.Active, ReviewHistory: []review.Snapshot{{Round: 1, Attempt: 1, Report: &review.Report{
			Criteria: []acceptance.Verdict{{ID: "AC1", Verdict: "met", Note: "Pages hold 50 orders."}, {ID: "AC2", Verdict: "not_met", Note: "Cursor exposes database IDs."}, {ID: "AC3", Verdict: "unverified", Note: "No load test ran."}},
			Findings: []review.Finding{{ID: "F1", Severity: "blocking", Title: "Cursor leaks IDs", Details: "Encode the cursor."}, {ID: "F2", Severity: "note", Title: "Naming", Details: "Consider renaming pageSize."}}}}}}},
			map[string]string{"Pages hold 50 orders.": "success", "Cursor exposes database IDs.": "error", "No load test ran.": "warning", "Encode the cursor.": "error", "Consider renaming pageSize.": "info"}},
		{"fix history", runView{Run: workflow.Run{State: workflow.Active, FixHistory: []review.FixSnapshot{{Round: 1, Attempt: 1,
			Findings: []review.Finding{{ID: "F1", Severity: "blocking", Title: "Cursor leaks IDs", Details: "Encode the opaque cursor."}},
			CI: &ci.Snapshot{RepairCause: "ci.check_failed", Evidence: ci.Evidence{Checks: []ci.Check{
				{Name: "unit tests", State: "passed", Conclusion: "success"}, {Name: "lint", State: "failed", Conclusion: "failure", Excerpt: "orders.go:88: unused variable"}}}}}}}},
			map[string]string{"Encode the opaque cursor.": "error", "unit tests · success": "success", "orders.go:88: unused variable": "error"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.run.ID, tc.run.Repository, tc.run.IssueNumber = "run-1", "owner/repo", 7
			assertAlerts(t, renderContent(t, pageData{Page: "run", Path: "/runs/run-1", Title: "Run detail", Run: &tc.run}), tc.want)
		})
	}
}
