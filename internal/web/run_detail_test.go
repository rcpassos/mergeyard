package web

import (
	"regexp"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/escalation"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/maintenance"
	"github.com/rcpassos/mergeyard/internal/review"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func renderRun(t *testing.T, run runView) string {
	t.Helper()
	return renderContent(t, pageData{Page: "run", Path: "/runs/" + run.ID, Title: "Run detail", Token: "csrf-1", Run: &run})
}

// detailsOpening returns the opening tag of the disclosure with id, or "".
func detailsOpening(body, id string) string {
	return regexp.MustCompile(`<details id="` + regexp.QuoteMeta(id) + `"[^>]*>`).FindString(body)
}

func disclosure(body, id string) string {
	start := strings.Index(body, `<details id="`+id+`"`)
	if start < 0 {
		return ""
	}
	end := strings.Index(body[start:], "</details>")
	if end < 0 {
		return ""
	}
	return body[start : start+end]
}

func withoutDisclosures(body string) string {
	return regexp.MustCompile(`(?s)<details.*?</details>`).ReplaceAllString(body, "")
}

func TestRunDetailHeaderHoldsIdentityStatusLinksAndEligibleActions(t *testing.T) {
	run := runView{
		Run: workflow.Run{ID: "run-1", Repository: "owner/repo", IssueNumber: 7, State: workflow.NeedsAttention,
			RunMetadata: workflow.RunMetadata{PRNumber: 12}, CreatedAt: "2026-10-21T08:00:00Z", UpdatedAt: "2026-10-21T09:00:00Z"},
		Title:    "Paginate orders",
		IssueURL: "https://github.com/owner/repo/issues/7", PRURL: "https://github.com/owner/repo/pull/12",
		ProcessSession: "mergeyard-run-1", Worktree: "/tmp/worktrees/issue-7",
		CanTakeover: true, CanHandback: true, CanRetry: true, CanStop: true,
	}
	body := renderRun(t, run)

	header := body[strings.Index(body, `<header id="run-header"`):]
	header = header[:strings.Index(header, "</header>")]
	for _, want := range []string{
		"sticky", "top-0", "owner/repo #7 · Paginate orders", "Needs attention",
		`href="https://github.com/owner/repo/issues/7"`, "Open issue",
		`href="https://github.com/owner/repo/pull/12"`, "Open PR",
		"Copy watch command", "Copy worktree path",
		"Take over run", "Hand back run", "Retry run", "Stop run", `value="csrf-1"`,
	} {
		if !strings.Contains(header, want) {
			t.Errorf("sticky header is missing %q: %s", want, header)
		}
	}
	if strings.Count(header, "opens in a new tab") != 2 {
		t.Errorf("external links are not marked as external: %s", header)
	}
	if !strings.Contains(body, `form="retry-form"`) || !strings.Contains(body, `id="retry-form"`) {
		t.Error("the header's Retry action is not bound to the retry form")
	}
}

func TestRunDetailOffersOnlyEligibleActions(t *testing.T) {
	cases := []struct {
		name string
		run  runView
		want []string
		deny []string
	}{
		{"active", runView{Run: workflow.Run{State: workflow.Active}, CanStop: true}, []string{"Stop run"}, []string{"Take over run", "Hand back run", "Retry run"}},
		{"takeover", runView{Run: workflow.Run{State: workflow.Active}, CanTakeover: true, CanStop: true}, []string{"Take over run", "Stop run"}, []string{"Hand back run", "Retry run"}},
		{"manual", runView{Run: workflow.Run{State: workflow.Manual, TakeoverStatus: workflow.TakeoverManual}, CanHandback: true, CanStop: true}, []string{"Hand back run", "Stop run"}, []string{"Take over run", "Retry run"}},
		{"retry", runView{Run: workflow.Run{State: workflow.NeedsAttention}, CanRetry: true}, []string{"Retry run"}, []string{"Stop run", "Take over run", "Hand back run"}},
		{"stopping", runView{Run: workflow.Run{State: workflow.Active}, CanStop: true, Stopping: true}, []string{"Retry Stop"}, []string{"Stop run"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.run.ID, tc.run.Repository, tc.run.IssueNumber = "run-1", "owner/repo", 7
			body := renderRun(t, tc.run)
			header := body[strings.Index(body, `<header id="run-header"`):]
			header = header[:strings.Index(header, "</header>")]
			for _, want := range tc.want {
				if !strings.Contains(header, want) {
					t.Errorf("missing eligible action %q: %s", want, header)
				}
			}
			for _, deny := range tc.deny {
				if strings.Contains(header, deny) {
					t.Errorf("offered ineligible action %q: %s", deny, header)
				}
			}
		})
	}
}

func TestRunStepperMarksTheCurrentLifecycleStep(t *testing.T) {
	cases := []struct {
		name string
		run  runView
		step string
	}{
		{"claiming", runView{Run: workflow.Run{State: workflow.Claiming}}, "Claimed"},
		{"preparing", runView{Run: workflow.Run{State: workflow.Preparing}}, "Claimed"},
		{"implement", runView{Run: workflow.Run{State: workflow.Active, Phase: workflow.Implement}}, "Implement"},
		{"review", runView{Run: workflow.Run{State: workflow.Active, Phase: workflow.Review, RunMetadata: workflow.RunMetadata{ReviewRound: 2}}}, "Review · round 2"},
		{"fix", runView{Run: workflow.Run{State: workflow.Active, Phase: workflow.Fix, RunMetadata: workflow.RunMetadata{ReviewRound: 2}}}, "Fix"},
		{"ci", runView{Run: workflow.Run{State: workflow.WaitingForCI, Phase: workflow.Review}}, "CI"},
		{"harness wait", runView{Run: workflow.Run{State: workflow.WaitingForHarness, Phase: workflow.Review, RunMetadata: workflow.RunMetadata{ReviewRound: 1}}}, "Review · round 1"},
		{"manual", runView{Run: workflow.Run{State: workflow.Manual, Phase: workflow.Implement, TakeoverStatus: workflow.TakeoverManual}}, "Implement"},
		{"ready", runView{Run: workflow.Run{State: workflow.ReadyToMerge, Phase: workflow.Review}}, "Ready"},
		{"merged", runView{Run: workflow.Run{State: workflow.Completed, Phase: workflow.Review,
			Merge: &maintenance.Snapshot{ObservedAt: "2026-10-21T10:00:00Z"}}}, "Merged"},
		{"early merge", runView{Run: workflow.Run{State: workflow.Active, Phase: workflow.Review,
			Merge: &maintenance.Snapshot{ObservedAt: "2026-10-21T10:00:00Z", Early: true}}}, "Merged"},
	}
	current := regexp.MustCompile(`<li class="step[^"]*" aria-current="step"><span[^>]*>([^<]+)</span></li>`)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.run.ID, tc.run.Repository, tc.run.IssueNumber = "run-1", "owner/repo", 7
			body := renderRun(t, tc.run)
			match := current.FindStringSubmatch(body)
			if match == nil || match[1] != tc.step {
				t.Fatalf("current step = %q, want %q: %s", match, tc.step, body)
			}
			for _, label := range []string{"Claimed", "Implement", "Review", "Fix", "CI", "Ready", "Merged"} {
				if !strings.Contains(body, ">"+label) && !strings.Contains(body, label+" · round") {
					t.Errorf("stepper is missing %q: %s", label, body)
				}
			}
		})
	}
}

func TestRunDetailCurrentAreaHoldsWhatNeedsAttention(t *testing.T) {
	run := runView{Run: workflow.Run{
		ID: "run-1", Repository: "owner/repo", IssueNumber: 7, State: workflow.WaitingForHarness, Phase: workflow.Review,
		HumanAction:      &fault.HumanAction{Decision: "Choose which migration wins."},
		LastErrorCode:    "phase.failed",
		LastErrorMessage: "Implementer exited with status 2.",
		HarnessWait:      &workflow.HarnessWait{Harness: "claude", Reason: "temporary_limit", ResetAt: resetAt},
	}, Stopping: true}
	body := renderRun(t, run)
	if !strings.Contains(body, `<section id="run-current"`) {
		t.Fatalf("current area is missing: %s", body)
	}
	for _, want := range []string{"Human action required", "Choose which migration wins.", "Last error", "Implementer exited with status 2.", "Waiting for claude until", "Stop requested."} {
		if !strings.Contains(body, want) {
			t.Errorf("current area is missing %q: %s", want, body)
		}
	}

	ready := runView{Run: workflow.Run{ID: "run-2", Repository: "owner/repo", IssueNumber: 8, State: workflow.ReadyToMerge, Phase: workflow.Review,
		Escalation: &escalation.Snapshot{EvaluationWarning: "Escalation rules could not be evaluated."}}}
	if body := renderRun(t, ready); !strings.Contains(body, "Waiting for your merge") || !strings.Contains(body, "Escalation rules could not be evaluated.") {
		t.Errorf("ready run does not escalate for the merge: %s", body)
	}

	quiet := runView{Run: workflow.Run{ID: "run-3", Repository: "owner/repo", IssueNumber: 9, State: workflow.Active, Phase: workflow.Implement}}
	if body := renderRun(t, quiet); !strings.Contains(body, "Nothing needs you right now.") {
		t.Errorf("quiet run has no empty current state: %s", body)
	}
}

func TestRunHistoryKeepsLatestOfEachKindOpenAndEveryDisclosureIdentified(t *testing.T) {
	run := richRun()
	run.RoleSettings = &config.RunRoleSettings{Implementer: config.RoleSettings{Agent: "claude"}}
	run.ImplementHistory = []workflow.ImplementSnapshot{{Attempt: 1}, {Attempt: 2}}
	run.ReviewHistory = []review.Snapshot{{Round: 1, Attempt: 1}, {Round: 2, Attempt: 1}}
	run.FixHistory = []review.FixSnapshot{{Round: 1, Attempt: 1}, {Round: 2, Attempt: 1}}
	run.Publications = []review.Publication{{Phase: "review", Round: 1, Attempt: 1, State: "published"}, {Phase: "fix", Round: 2, Attempt: 1, State: "published"}}
	run.SessionRecoveries = []workflow.SessionRecovery{{Phase: workflow.Review, Round: 1, PreviousSessionID: "old-1"}, {Phase: workflow.Fix, Round: 2, PreviousSessionID: "old-2"}}
	run.CreditProbes = []workflow.CreditProbe{{Harness: "claude", RequestedAt: "2026-10-21T08:00:00Z"}, {Harness: "codex", RequestedAt: "2026-10-21T09:00:00Z"}}
	run.HarnessWaitHistory = []workflow.HarnessWait{{Harness: "claude", Reason: "temporary_limit"}, {Harness: "codex", Reason: "credits_exhausted"}}
	run.Retries = []workflow.RetrySnapshot{{ID: 1, RequestedAt: "2026-10-21T08:00:00Z"}, {ID: 2, RequestedAt: "2026-10-21T09:00:00Z"}}
	run.Handbacks = []workflow.HandbackSnapshot{{ID: 1, RequestedAt: "2026-10-21T08:00:00Z"}, {ID: 2, RequestedAt: "2026-10-21T09:00:00Z"}}
	body := renderRun(t, run)

	latest := map[string]string{
		"history-implement-2":            "history-implement-1",
		"history-review-2-1":             "history-review-1-1",
		"history-fix-2-1":                "history-fix-1-1",
		"history-ci":                     "",
		"history-publication-1":          "history-publication-0",
		"history-session-recovery-old-2": "history-session-recovery-old-1",
		"history-credit-probe-1":         "history-credit-probe-0",
		"history-harness-wait-1":         "history-harness-wait-0",
		"history-retry-2":                "history-retry-1",
		"history-handback-2":             "history-handback-1",
		"run-facts":                      "",
		"role-settings":                  "",
	}
	for id, older := range latest {
		opening := detailsOpening(body, id)
		if opening == "" {
			t.Errorf("history disclosure %q is missing", id)
			continue
		}
		if !strings.Contains(opening, " open") {
			t.Errorf("latest disclosure %q is collapsed: %s", id, opening)
		}
		if older != "" {
			if opening := detailsOpening(body, older); opening == "" {
				t.Errorf("older disclosure %q is missing", older)
			} else if strings.Contains(opening, " open") {
				t.Errorf("older disclosure %q is open: %s", older, opening)
			}
		}
	}
	for _, opening := range regexp.MustCompile(`<details\b[^>]*>`).FindAllString(body, -1) {
		if !strings.Contains(opening, `id="`) {
			t.Errorf("disclosure has no stable ID: %s", opening)
		}
	}
}

func TestRunDetailMovesLongGuidanceBehindDisclosures(t *testing.T) {
	run := runView{Run: workflow.Run{
		ID: "run-1", Repository: "owner/repo", IssueNumber: 7, State: workflow.WaitingForHarness, Phase: workflow.Review,
		HarnessWait: &workflow.HarnessWait{Harness: "codex", Reason: "credits_exhausted"},
	}, CanHandback: true, CanRetry: true}
	body := renderRun(t, run)
	for id, want := range map[string]string{
		"handback-learn-more":        "Exit the interactive harness before handback.",
		"retry-learn-more":           "Guidance stays in every later agent input",
		"credit-recovery-learn-more": "Retry can use quota to probe credit recovery",
	} {
		block := disclosure(body, id)
		if block == "" || !strings.Contains(block, want) {
			t.Errorf("guidance %q is not inside disclosure %q: %s", want, id, block)
		}
	}
	visible := withoutDisclosures(body)
	for _, guidance := range []string{"Exit the interactive harness", "Guidance stays in every later agent input", "Retry can use quota"} {
		if strings.Contains(visible, guidance) {
			t.Errorf("guidance %q renders inline", guidance)
		}
	}
}

func TestRunDetailShortensSHAsAndSessionIDsWithFullCopyValue(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	session := "12345678-1234-1234-1234-123456789abc"
	run := runView{Run: workflow.Run{
		ID: "run-1", Repository: "owner/repo", IssueNumber: 7, State: workflow.WaitingForCI, Phase: workflow.Review,
		RunMetadata:   workflow.RunMetadata{ApprovedSHA: sha, ReviewRound: 1},
		CI:            &ci.Snapshot{SHA: sha, CurrentHead: sha, Evidence: ci.Evidence{MergeSHA: sha}},
		ReviewHistory: []review.Snapshot{{Round: 1, Attempt: 1, SessionID: session, TargetSHA: sha}},
	}}
	body := renderRun(t, run)
	if !strings.Contains(body, `data-copy="`+sha+`"`) || !strings.Contains(body, "0123456…") {
		t.Errorf("SHA is not shortened with its full copy value: %s", body)
	}
	if !strings.Contains(body, `data-copy="`+session+`"`) || !strings.Contains(body, "12345678…") {
		t.Errorf("session ID is not shortened with its full copy value: %s", body)
	}
	for _, visible := range []string{">" + sha + "<", ">" + session + "<"} {
		if strings.Contains(body, visible) {
			t.Errorf("full identifier renders as visible text: %s", visible)
		}
	}
}

func TestRunDetailMarksExternalLinks(t *testing.T) {
	run := runView{Run: workflow.Run{
		ID: "run-1", Repository: "owner/repo", IssueNumber: 7, State: workflow.ReadyToMerge, Phase: workflow.Review,
		CI:           &ci.Snapshot{Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build", State: "passed", URL: "https://github.com/owner/repo/checks/1"}}}},
		Publications: []review.Publication{{Phase: "review", Round: 1, Attempt: 1, CommentURL: "https://github.com/owner/repo/pull/12#issuecomment-1"}},
	}, IssueURL: "https://github.com/owner/repo/issues/7", PRURL: "https://github.com/owner/repo/pull/12"}
	body := renderRun(t, run)
	for _, url := range []string{"https://github.com/owner/repo/issues/7", "https://github.com/owner/repo/pull/12", "https://github.com/owner/repo/checks/1", "https://github.com/owner/repo/pull/12#issuecomment-1"} {
		at := strings.Index(body, `href="`+url+`"`)
		if at < 0 {
			t.Errorf("link %q is missing", url)
			continue
		}
		link := body[at:]
		link = link[:strings.Index(link, "</a>")]
		if !strings.Contains(link, "opens in a new tab") || !strings.Contains(link, "icon-external-link") {
			t.Errorf("external link %q is not marked: %s", url, link)
		}
	}
}

// The copy control carries the exact value even when it contains characters
// that would otherwise break out of an attribute or inject markup.
func TestShortCopyEscapesItsFullValue(t *testing.T) {
	body := string(shortCopy(`sha"<script>&'`, 3, `label "quoted"`))
	if strings.Contains(body, "<script>") || strings.Contains(body, `data-copy="sha"`) {
		t.Fatalf("full value broke out of its attribute: %s", body)
	}
	for _, want := range []string{
		`data-copy="sha&#34;&lt;script&gt;&amp;&#39;"`,
		`title="sha&#34;&lt;script&gt;&amp;&#39;"`,
		`aria-label="Copy label &#34;quoted&#34;: sha&#34;&lt;script&gt;&amp;&#39;"`,
		">sha…</button>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("copy control is missing %q: %s", want, body)
		}
	}
}

func TestCurrentAreaMattersOnlyWhenItHasSomethingToShow(t *testing.T) {
	cases := map[string]runView{
		"merge":                 {Run: workflow.Run{Merge: &maintenance.Snapshot{ObservedAt: "2026-10-21T10:00:00Z"}}},
		"ready":                 {Run: workflow.Run{State: workflow.ReadyToMerge}},
		"review queued":         {ReviewPending: true},
		"stopping":              {Stopping: true},
		"takeover prerequisite": {TakeoverPrerequisite: "Takeover needs a prepared worktree."},
		"takeover requested":    {Run: workflow.Run{TakeoverStatus: workflow.TakeoverRequested}},
		"resume command":        {ResumeCommand: "mergeyard takeover -- run-1"},
		"human action":          {Run: workflow.Run{HumanAction: &fault.HumanAction{Decision: "Choose."}}},
		"last error":            {Run: workflow.Run{LastErrorCode: "phase.failed"}},
		"stopped publication":   {StoppedPublications: []scheduler.StoppedPublication{{Label: "Review report"}}},
		"harness wait":          {Run: workflow.Run{State: workflow.WaitingForHarness, HarnessWait: &workflow.HarnessWait{Harness: "claude"}}},
		"retry":                 {CanRetry: true},
		"handback":              {CanHandback: true},
	}
	for name, run := range cases {
		if !currentMatters(&run) {
			t.Errorf("%s does not populate the current area", name)
		}
	}
	quiet := runView{Run: workflow.Run{State: workflow.Active, HarnessWait: &workflow.HarnessWait{Harness: "claude"}}}
	if currentMatters(&quiet) {
		t.Error("an active run with only a stale harness wait populates the current area")
	}
	if currentMatters(nil) {
		t.Error("a missing run populates the current area")
	}
}
