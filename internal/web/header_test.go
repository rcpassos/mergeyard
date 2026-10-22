package web

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func renderTemplate(t *testing.T, name string, data pageData) string {
	t.Helper()
	set, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	if err := set.ExecuteTemplate(&body, name, data); err != nil {
		t.Fatal(err)
	}
	return body.String()
}

var markupTag = regexp.MustCompile(`<[^>]+>`)

var pillSummary = regexp.MustCompile(`(?s)<summary[^>]*\bclass="[^"]*\bbadge (badge-\w+)[^"]*"[^>]*>(.*?)</summary>`)

// pills maps each harness pill's visible text, tags stripped, to its badge
// variant.
func pills(body string) map[string]string {
	result := map[string]string{}
	for _, match := range pillSummary.FindAllStringSubmatch(body, -1) {
		result[strings.Join(strings.Fields(markupTag.ReplaceAllString(match[2], " ")), " ")] = match[1]
	}
	return result
}

func headerData(page string) pageData {
	reset := time.Date(2026, 10, 21, 14, 30, 0, 0, time.UTC)
	return pageData{Page: page, Path: "/", Title: "Dashboard", Token: "csrf-1", Groups: emptyGroups(), Harnesses: []scheduler.HarnessAvailability{
		{Harness: "claude", Available: true, TemporaryLimitDetection: true},
		{Harness: "codex", Reason: "usage_limited", ResetAt: reset, ResetTimeSource: "reported", Source: "fixture"},
		{Harness: "gemini", Reason: "credits_exhausted", Source: "stderr", CheckEligible: true,
			Check: &workflow.HarnessCheck{Status: "failed", Result: "still exhausted"}},
		{Harness: "pi", Reason: "credits_exhausted", ProbeID: "probe-1", ProbeAttemptID: "attempt-1", ProbeRunID: "run-9"},
		{Harness: "amp", Reason: "credits_exhausted", ProbeID: "probe-2"},
	}}
}

func TestHeaderShowsOnePillPerHarness(t *testing.T) {
	for _, page := range []string{"dashboard", "queue", "settings"} {
		body := renderTemplate(t, "layout", headerData(page))
		got := pills(body)
		want := map[string]string{
			"claude · Available": "badge-success",
			"codex · Usage limited · resets 2026-10-21 14:30 UTC": "badge-warning",
			"gemini · Out of credits":                             "badge-warning",
			"pi · Recovery probe running":                         "badge-info",
			"amp · Recovery probe reserved":                       "badge-info",
		}
		for text, variant := range want {
			if got[text] != variant {
				t.Errorf("%s: pill %q has variant %q, want %q (pills %v)", page, text, got[text], variant, got)
			}
		}
		if len(got) != len(want) {
			t.Errorf("%s: %d pills, want %d: %v", page, len(got), len(want), got)
		}
		if !strings.Contains(body, `<time datetime="2026-10-21T14:30:00Z" data-time="until">`) {
			t.Errorf("%s: reset time is not a time element", page)
		}
	}
}

func TestHarnessPillDropdownHoldsDetailsAndCheckAvailability(t *testing.T) {
	body := renderTemplate(t, "layout", headerData("dashboard"))
	for _, text := range []string{
		"Waiting until", "reported", "fixture",
		"Credits exhausted · explicit recovery required", "stderr",
		"Temporary-limit detection unavailable; usage limits consume the ordinary attempt budget.",
		"Availability check failed · still exhausted",
		"for run run-9", "independent availability check",
	} {
		if !strings.Contains(body, text) {
			t.Errorf("dropdown is missing %q", text)
		}
	}
	// Only the eligible harness offers the check, with its quota notice. The
	// confirmation modal repeats the action label, so count forms instead.
	if count := strings.Count(body, `action="/harnesses/gemini/check"`); count != 1 {
		t.Fatalf("Check availability offered %d times", count)
	}
	form := body[strings.Index(body, `action="/harnesses/gemini/check"`):]
	form = form[:strings.Index(form, "</form>")]
	if !strings.Contains(form, "uses account quota") || !strings.Contains(form, "no engineering work") || !strings.Contains(form, `value="csrf-1"`) {
		t.Fatalf("check form lacks its quota notice or token: %s", form)
	}
}

func TestPageContentCarriesNoHarnessAvailability(t *testing.T) {
	content := renderContent(t, headerData("dashboard"))
	if strings.Contains(content, "Harness availability") || strings.Contains(content, "Waiting until") {
		t.Fatalf("page content still carries harness availability: %s", content)
	}
}

// A live update swaps the page content; the same response refreshes the
// header pills out of band so they never go stale.
func TestContentSwapRefreshesHeaderPills(t *testing.T) {
	body := renderTemplate(t, "swap", headerData("dashboard"))
	if !strings.Contains(body, `id="harness-pills" hx-swap-oob="innerHTML"`) || pills(body)["codex · Usage limited · resets 2026-10-21 14:30 UTC"] == "" {
		t.Fatalf("swap response does not refresh the pills: %s", body)
	}
	if !strings.Contains(body, `<main id="content"`) {
		t.Fatalf("swap response lacks the page content: %s", body)
	}
}

func TestHeaderShowsConnectionDotAndToastRegion(t *testing.T) {
	body := renderTemplate(t, "layout", headerData("dashboard"))
	dot := regexp.MustCompile(`id="connection"[^>]*data-tip="Connecting to live updates…"`)
	if !dot.MatchString(body) || !regexp.MustCompile(`<span[^>]*class="sr-only"[^>]*>Connecting to live updates…</span>`).MatchString(body) {
		t.Errorf("connection is not a dot with an accessible label: %s", body)
	}
	if !strings.Contains(body, `id="toasts"`) {
		t.Errorf("no toast region: %s", body)
	}
	if strings.Contains(body, `id="action-error"`) || strings.Contains(body, `id="copy-status"`) {
		t.Errorf("inline feedback paragraphs remain: %s", body)
	}
	if !strings.Contains(body, `<dialog id="copy-fallback"`) {
		t.Errorf("no copy fallback modal: %s", body)
	}
}
