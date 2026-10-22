package scheduler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// These expectations describe the owner's timeline, including the phase and
// escalation prefixes whose categories do not simply repeat their prefix.
func TestTimelineReadableEvents(t *testing.T) {
	groups := []struct {
		category, icon string
		events         map[string]string
	}{
		{"run", "activity", map[string]string{
			"run.claimed":             "Run claimed",
			"run.preparing":           "Run preparing",
			"run.needs_attention":     "Run needs attention",
			"run.stop_requested":      "Run stop requested",
			"run.stop_leftovers":      "Run stop leftovers",
			"run.stopped":             "Run stopped",
			"run.failed":              "Run failed",
			"run.takeover_requested":  "Run takeover requested",
			"run.retry_requested":     "Run retry requested",
			"run.retry_rejected":      "Run retry rejected",
			"run.handback_requested":  "Run handback requested",
			"run.handback_inspected":  "Run handback inspected",
			"run.handback_committed":  "Run handback committed",
			"run.handback_pending":    "Run handback pending",
			"run.completed":           "Run completed",
			"run.manual":              "Run manual",
			"run.handed_back":         "Run handed back",
			"run.waiting_for_harness": "Run waiting for harness",
			"escalation.updated":      "Escalation updated",
			"run.future_event":        "Run future event",
		}},
		{"phase", "layers", map[string]string{
			"review.approval_invalidated": "Review approval invalidated",
			"review.completed":            "Review completed",
			"review.restored":             "Review restored",
			"fix.completed":               "Fix completed",
			"fix.committed":               "Fix committed",
			"fix.pushed":                  "Fix pushed",
			"phase.started":               "Phase started",
			"phase.attempt_started":       "Phase attempt started",
			"phase.completed":             "Phase completed",
			"phase.failed":                "Phase failed",
		}},
		{"pr", "git-pull-request", map[string]string{"pr.merge_observed": "PR merge observed",
			"pr.readiness_started": "PR readiness started",
			"pr.created":           "PR created",
			"pr.ready_for_review":  "PR ready for review"}},
		{"ci", "list-checks", map[string]string{"ci.updated": "CI updated"}},
		{"harness", "terminal", map[string]string{
			"harness.usage_limited":         "Harness usage limited",
			"harness.credits_exhausted":     "Harness credits exhausted",
			"harness.credit_recovered":      "Harness credit recovered",
			"harness.probe_reserved":        "Harness probe reserved",
			"harness.probe_released":        "Harness probe released",
			"harness.available":             "Harness available",
			"harness.check_started":         "Harness check started",
			"harness.check_updated":         "Harness check updated",
			"harness.check_completed":       "Harness check completed",
			"harness.session_discovered":    "Harness session discovered",
			"harness.session_resume_failed": "Harness session resume failed",
		}},
		{"merge", "git-merge", map[string]string{"merge.cleanup_updated": "Merge cleanup updated"}},
		{"publication", "send", map[string]string{"publication.pending": "Publication pending",
			"publication.warning":   "Publication warning",
			"publication.published": "Publication published",
			"publication.stopped":   "Publication stopped",
			"publication.requeued":  "Publication requeued"}},
		{"reconcile", "refresh-cw", map[string]string{"reconcile.repository_failed": "Reconcile repository failed",
			"reconcile.issue_failed": "Reconcile issue failed",
			"reconcile.run_failed":   "Reconcile run failed"}},
		{"scheduler", "calendar-clock", map[string]string{"scheduler.paused": "Scheduler paused",
			"scheduler.resumed": "Scheduler resumed"}},
		{"other", "info", map[string]string{"telemetry.new_signal": "Telemetry new signal",
			"mystery":                       "Mystery",
			"...__":                         "Unknown event",
			"custom.<script>event</script>": "Custom &lt;script&gt;event&lt;/script&gt;"}},
	}
	ctx := context.Background()
	s, rt := configured(t, &fakeGitHub{}, "repositories:\n  - repo: owner/repo\n", rejectingGit{})
	run, err := rt.Workflow.Transition(ctx, "timeline", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7})
	if err != nil {
		t.Fatal(err)
	}
	type expected struct {
		id                         int64
		kind, name, category, icon string
	}
	var wants []expected
	covered := map[string]bool{}
	for _, group := range groups {
		for kind, name := range group.events {
			event, err := rt.Events.Publish(ctx, events.Draft{RunID: run.ID, Type: kind, Payload: json.RawMessage(`{"note":"<script>raw & details</script>"}`)})
			if err != nil {
				t.Fatal(err)
			}
			wants = append(wants, expected{event.ID, kind, name, group.category, group.icon})
			covered[kind] = true
		}
	}
	// Seed verbatim JSON as stored by older/external event writers, including
	// literal HTML characters that must be escaped at the HTTP surface.
	if _, err := rt.DB.Exec("UPDATE events SET payload_json=? WHERE run_id=?", `{"note":"<script>raw & details</script>"}`, run.ID); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("../../web/src/app.js")
	if err != nil {
		t.Fatal(err)
	}
	refresh := regexp.MustCompile(`(?s)const refreshEvents = \[(.*?)\];`).FindSubmatch(source)
	if len(refresh) != 2 {
		t.Fatal("refresh event list not found")
	}
	for _, match := range regexp.MustCompile(`'([^']+)'`).FindAllSubmatch(refresh[1], -1) {
		if !covered[string(match[1])] {
			t.Errorf("refresh event %s needs a timeline expectation", match[1])
		}
	}
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, dashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("dashboard status %d: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, category := range []string{"all", "run", "phase", "pr", "ci", "harness", "merge", "publication", "reconcile", "scheduler", "other"} {
		if !strings.Contains(body, `data-timeline-filter="`+category+`"`) {
			t.Errorf("missing category filter %s", category)
		}
	}
	entries := regexp.MustCompile(`(?s)<li\b[^>]*>.*?</li>`).FindAllString(body, -1)
	for _, want := range wants {
		entry := ""
		for _, item := range entries {
			if strings.Contains(item, fmt.Sprintf(`id="timeline-event-%d"`, want.id)) {
				entry = item
				break
			}
		}
		summary, _, _ := strings.Cut(entry, "<details")
		if strings.Contains(summary, ">"+want.kind+"<") {
			t.Error("raw event type shown outside disclosure")
		}
		for _, text := range []string{">" + want.name + "</strong>", `data-category="` + want.category + `"`, "icon-" + want.icon, `role="img"`, `data-time="since"`, "&lt;script&gt;raw &amp; details&lt;/script&gt;"} {
			if !strings.Contains(entry, text) {
				t.Errorf("%s missing %q", want.name, text)
			}
		}
		if strings.Contains(entry, "<script>") {
			t.Errorf("%s raw payload is not escaped", want.name)
		}
	}
}
