package web

import (
	"html"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/maintenance"
	"github.com/rcpassos/mergeyard/internal/review"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

var resetAt = time.Date(2026, 10, 21, 14, 30, 0, 0, time.UTC)

// badgeVariant reports the badge classes of the first status badge whose text
// starts with label, or "" when no badge shows it.
func badgeVariant(body, label string) string {
	match := regexp.MustCompile(`<span class="badge ([a-z -]+)">` + regexp.QuoteMeta(html.EscapeString(label))).FindStringSubmatch(body)
	if match == nil {
		return ""
	}
	return match[1]
}

func TestRunsShowAHumanStatusBadge(t *testing.T) {
	cases := []struct {
		name, label, variant string
		run                  runView
	}{
		{"claiming", "Starting", "badge-neutral", runView{Run: workflow.Run{State: workflow.Claiming}}},
		{"preparing", "Starting", "badge-neutral", runView{Run: workflow.Run{State: workflow.Preparing}}},
		{"implementing", "Implementing", "badge-info", runView{Run: workflow.Run{State: workflow.Active, Phase: workflow.Implement}}},
		{"reviewing", "Reviewing · round 2", "badge-info", runView{Run: workflow.Run{State: workflow.Active, Phase: workflow.Review, RunMetadata: workflow.RunMetadata{ReviewRound: 2}}}},
		{"fixing", "Fixing · round 2", "badge-info", runView{Run: workflow.Run{State: workflow.Active, Phase: workflow.Fix, RunMetadata: workflow.RunMetadata{ReviewRound: 2}}}},
		{"review queued", "Review queued", "badge-info", runView{Run: workflow.Run{State: workflow.Active, Phase: workflow.Review}, ReviewPending: true}},
		{"waiting for CI", "Waiting for CI", "badge-info", runView{Run: workflow.Run{State: workflow.WaitingForCI, Phase: workflow.Review}}},
		{"manual", "You're driving", "badge-accent", runView{Run: workflow.Run{State: workflow.Manual, Phase: workflow.Implement, TakeoverStatus: workflow.TakeoverManual}}},
		{"usage limited", "Usage limited · resets ", "badge-warning", runView{Run: workflow.Run{State: workflow.WaitingForHarness, Phase: workflow.Review,
			HarnessWait: &workflow.HarnessWait{Harness: "claude", Reason: "temporary_limit", ResetAt: resetAt}}}},
		{"out of credits", "Out of credits", "badge-warning", runView{Run: workflow.Run{State: workflow.WaitingForHarness, Phase: workflow.Implement,
			HarnessWait: &workflow.HarnessWait{Harness: "codex", Reason: "credits_exhausted"}}}},
		{"needs attention", "Needs attention", "badge-error", runView{Run: workflow.Run{State: workflow.NeedsAttention, Phase: workflow.Fix}}},
		{"ready to merge", "Ready to merge", "badge-success", runView{Run: workflow.Run{State: workflow.ReadyToMerge, Phase: workflow.Review}}},
		{"merged", "Merged", "badge-success badge-outline", runView{Run: workflow.Run{State: workflow.Completed, Phase: workflow.Review,
			Merge: &maintenance.Snapshot{ObservedAt: "2026-10-21T10:00:00Z"}}}},
		// An early merge is saved before the owned phase exits, so the run can
		// still be ACTIVE; the observed merge wins.
		{"merged before phase exit", "Merged", "badge-success badge-outline", runView{Run: workflow.Run{State: workflow.Active, Phase: workflow.Review,
			RunMetadata: workflow.RunMetadata{ReviewRound: 2}, Merge: &maintenance.Snapshot{ObservedAt: "2026-10-21T10:00:00Z", Early: true}}}},
		{"failed", "Failed", "badge-error badge-outline", runView{Run: workflow.Run{State: workflow.Failed, Phase: workflow.Implement}}},
		{"stopped", "Stopped", "badge-neutral badge-outline", runView{Run: workflow.Run{State: workflow.Stopped}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.run.ID, tc.run.Repository, tc.run.IssueNumber, tc.run.Title = "run-1", "owner/repo", 7, "Paginate orders"
			row := pageData{Page: "dashboard", Path: "/", Title: "Dashboard", Sections: emptySections()}
			row.Sections[runningSection].Runs = []runView{tc.run}
			detail := pageData{Page: "run", Path: "/runs/run-1", Title: "Run detail", Run: &tc.run}
			for page, data := range map[string]pageData{"run row": row, "run detail header": detail} {
				body := renderContent(t, data)
				if got := badgeVariant(body, tc.label); got != tc.variant {
					t.Errorf("%s: badge %q has variant %q, want %q", page, tc.label, got, tc.variant)
				}
			}
		})
	}

	// A timed limit names its reset as a machine-readable instant.
	limited := runView{Run: workflow.Run{ID: "run-1", State: workflow.WaitingForHarness, HarnessWait: &workflow.HarnessWait{Reason: "temporary_limit", ResetAt: resetAt}}}
	body := renderContent(t, pageData{Page: "run", Path: "/runs/run-1", Run: &limited})
	if !strings.Contains(body, `Usage limited · resets <time datetime="2026-10-21T14:30:00Z"`) {
		t.Errorf("usage-limited badge does not carry its reset time: %s", body)
	}
}

// richRun has every kind of history whose copy names a state or phase.
func richRun() runView {
	return runView{Run: workflow.Run{
		ID: "run-1", Repository: "owner/repo", IssueNumber: 7, State: workflow.WaitingForHarness, Phase: workflow.Review,
		UpdatedAt:   "2026-10-21T09:00:00.123Z",
		HarnessWait: &workflow.HarnessWait{Harness: "claude", Reason: "temporary_limit", Phase: workflow.Review, Round: 1, ResetAt: resetAt},
		HarnessWaitHistory: []workflow.HarnessWait{
			{Harness: "claude", Reason: "temporary_limit", Phase: workflow.Review, Round: 1, ResetAt: resetAt},
			{Harness: "codex", Reason: "credits_exhausted", Phase: workflow.Fix, Round: 2},
		},
		Handbacks:         []workflow.HandbackSnapshot{{RequestedAt: "2026-10-21T08:00:00Z", NextState: workflow.Active, NextPhase: workflow.Review, Round: 2}},
		Retries:           []workflow.RetrySnapshot{{RequestedAt: "2026-10-21T08:30:00Z", NextState: workflow.WaitingForHarness, NextPhase: workflow.Fix, Round: 2, Deadline: "2026-10-21T16:00:00Z"}},
		CreditProbes:      []workflow.CreditProbe{{Harness: "codex", Status: "completed", RequestedAt: "2026-10-21T08:40:00Z", EndedAt: "2026-10-21T08:41:00Z"}},
		SessionRecoveries: []workflow.SessionRecovery{{Phase: workflow.Fix, Role: "implementer", Round: 2, PreviousSessionID: "old"}},
		Publications:      []review.Publication{{Phase: "review", Round: 1, Attempt: 1, State: "published"}},
		CI:                &ci.Snapshot{Deadline: resetAt},
	}, Round: 1, Attempt: 2}
}

// stripTimelineDetails drops the raw event details, the one place raw
// codes may appear. History disclosures keep their content in the check.
func stripTimelineDetails(body string) string {
	return regexp.MustCompile(`(?s)<details id="timeline-event-[^"]*".*?</details>`).ReplaceAllString(body, "")
}

var (
	rawState = regexp.MustCompile(`\b(CLAIMING|PREPARING|ACTIVE|WAITING_FOR_CI|WAITING_FOR_HARNESS|READY_TO_MERGE|MANUAL|NEEDS_ATTENTION|FAILED|STOPPED|COMPLETED)\b`)
	rawPhase = regexp.MustCompile(`([>/·,]\s*)(implement|review|fix)(\s*[</·,]|\s+report)`)
)

func TestPagesKeepRawStateAndPhaseCodesInTimelineDetails(t *testing.T) {
	run := richRun()
	ended := runView{Run: workflow.Run{ID: "run-2", Repository: "owner/repo", IssueNumber: 8, State: workflow.Failed, Phase: workflow.Implement}}
	dashboard := pageData{Page: "dashboard", Path: "/", Title: "Dashboard", Sections: emptySections(), Recent: []runView{ended}}
	dashboard.Sections[runningSection].Runs = []runView{run}
	queue := dashboard
	queue.Page, queue.Path, queue.Title = "queue", "/queue", "Queue"
	detail := pageData{Page: "run", Path: "/runs/run-1", Title: "Run detail", Run: &run,
		Timeline: []timelineEntry{{Type: "run.state_changed", Time: "2026-10-21T09:00:00Z", Details: `{"state":"READY_TO_MERGE","phase":"review"}`}}}

	for name, data := range map[string]pageData{"Dashboard": dashboard, "Queue": queue, "Run detail": detail} {
		body := renderContent(t, data)
		if name == "Run detail" && !strings.Contains(body, "READY_TO_MERGE") {
			t.Fatalf("timeline details lost the raw event: %s", body)
		}
		visible := stripTimelineDetails(body)
		if code := rawState.FindString(visible); code != "" {
			t.Errorf("%s shows raw state %q outside timeline details: %s", name, code, visible)
		}
		if code := rawPhase.FindString(visible); code != "" {
			t.Errorf("%s shows raw phase %q outside timeline details: %s", name, code, visible)
		}
	}
}

func TestRenderedTimesCarryTheirInstant(t *testing.T) {
	run := richRun()
	run.Merge = &maintenance.Snapshot{ObservedAt: "2026-10-21T10:00:00Z"}
	harnesses := []scheduler.HarnessAvailability{{Harness: "claude", ResetAt: resetAt, Reason: "temporary_limit"}, {Harness: "codex", Available: true}}
	dashboard := pageData{Page: "dashboard", Path: "/", Title: "Dashboard", Sections: emptySections(), Harnesses: harnesses, QueueTime: "2026-10-21T09:30:00Z"}
	dashboard.Sections[runningSection].Runs = []runView{run}
	detail := pageData{Page: "run", Path: "/runs/run-1", Title: "Run detail", Run: &run, Harnesses: harnesses,
		Timeline: []timelineEntry{{Type: "run.claimed", Time: "2026-10-21T07:00:00.000Z", Details: "{}"}}}

	timeElement := regexp.MustCompile(`<time\b[^>]*>`)
	withInstant := regexp.MustCompile(`^<time datetime="\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z" data-time="(since|until)">$`)
	for name, data := range map[string]pageData{"Dashboard": dashboard, "Run detail": detail} {
		body := renderContent(t, data)
		elements := timeElement.FindAllString(body, -1)
		if len(elements) == 0 {
			t.Fatalf("%s renders no times", name)
		}
		for _, element := range elements {
			if !withInstant.MatchString(element) {
				t.Errorf("%s renders a time without its instant: %s", name, element)
			}
		}
		// CI started at the zero time, which must never render.
		if strings.Contains(body, "0001-01-01") {
			t.Errorf("%s renders a zero time: %s", name, body)
		}
	}
	for want, kind := range map[string]string{
		`<time datetime="2026-10-21T09:00:00Z" data-time="since">`: "updated time",
		`<time datetime="2026-10-21T16:00:00Z" data-time="until">`: "retry deadline",
		`<time datetime="2026-10-21T14:30:00Z" data-time="until">`: "reset time",
		`<time datetime="2026-10-21T07:00:00Z" data-time="since">`: "timeline time",
	} {
		if body := renderContent(t, detail); !strings.Contains(body, want) {
			t.Errorf("run detail lacks the %s %s", kind, want)
		}
	}
}
