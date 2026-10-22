package web

import (
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// actionForm returns the whole form that posts to action.
func actionForm(t *testing.T, body, action string) string {
	t.Helper()
	marker := `action="` + action + `"`
	at := strings.Index(body, marker)
	if at < 0 {
		t.Fatalf("no form posts to %s: %s", action, body)
	}
	start := strings.LastIndex(body[:at], "<form")
	end := strings.Index(body[at:], "</form>")
	if start < 0 || end < 0 {
		t.Fatalf("malformed form for %s: %s", action, body)
	}
	return body[start : at+end+len("</form>")]
}

// modalDialog returns the dialog with id.
func modalDialog(t *testing.T, body, id string) string {
	t.Helper()
	start := strings.Index(body, `<dialog id="`+id+`"`)
	if start < 0 {
		t.Fatalf("no dialog %q: %s", id, body)
	}
	end := strings.Index(body[start:], "</dialog>")
	if end < 0 {
		t.Fatalf("malformed dialog %q: %s", id, body)
	}
	return body[start : start+end]
}

// assertDirectSubmission keeps every action form usable without the dashboard
// script: the same POST target and CSRF token as before.
func assertDirectSubmission(t *testing.T, form, action string) {
	t.Helper()
	for _, want := range []string{`method="post"`, `action="` + action + `"`, `name="csrf_token"`, `value="csrf-1"`} {
		if !strings.Contains(form, want) {
			t.Errorf("form for %s is missing %q: %s", action, want, form)
		}
	}
}

func assertLoading(t *testing.T, form string) {
	t.Helper()
	if !strings.Contains(form, "hx-disabled-elt=") {
		t.Errorf("form does not disable its control while pending: %s", form)
	}
	if !strings.Contains(form, "htmx-indicator") {
		t.Errorf("form has no loading indicator: %s", form)
	}
	if !strings.Contains(form, `type="submit"`) {
		t.Errorf("form has no submit button: %s", form)
	}
}

// Stop and Take over state what they interrupt before the form submits.
func TestStopAndTakeoverModalsStateTheirConsequences(t *testing.T) {
	run := runView{
		Run:         workflow.Run{ID: "run-1", Repository: "owner/repo", IssueNumber: 7, State: workflow.Active},
		CanStop:     true,
		CanTakeover: true,
	}
	body := renderRun(t, run)
	cases := []struct {
		action, dialog, consequence, detail string
	}{
		{"/runs/run-1/stop", "confirm-stop", "interrupts the current phase", "preserves the worktree, branch and pull request"},
		{"/runs/run-1/takeover", "confirm-takeover", "suspends automation", "stops the running processes"},
	}
	for _, tc := range cases {
		form := actionForm(t, body, tc.action)
		if !strings.Contains(form, `data-confirm="#`+tc.dialog+`"`) {
			t.Errorf("form for %s does not open %s: %s", tc.action, tc.dialog, form)
		}
		assertDirectSubmission(t, form, tc.action)

		dialog := modalDialog(t, body, tc.dialog)
		for _, want := range []string{
			`class="modal"`, `aria-labelledby="` + tc.dialog + `-title"`, `aria-describedby="` + tc.dialog + `-consequence"`,
			`value="cancel"`, `data-confirm-cancel`, `value="confirm"`, `data-confirm-accept`, "Cancel", tc.consequence, tc.detail,
		} {
			if !strings.Contains(dialog, want) {
				t.Errorf("dialog %s is missing %q: %s", tc.dialog, want, dialog)
			}
		}
	}

	// Finishing a requested takeover no longer starts one: say so.
	run.TakeoverStatus = workflow.TakeoverRequested
	dialog := modalDialog(t, renderRun(t, run), "confirm-takeover")
	if !strings.Contains(dialog, "finishes the takeover") || strings.Contains(dialog, "This suspends automation") {
		t.Errorf("finish-takeover dialog still describes starting a takeover: %s", dialog)
	}
}

// Check availability confirms that it spends account quota before submitting.
func TestCheckAvailabilityModalStatesItsQuotaUse(t *testing.T) {
	body := renderTemplate(t, "layout", headerData("dashboard"))
	form := actionForm(t, body, "/harnesses/gemini/check")
	if !strings.Contains(form, `data-confirm="#confirm-check-gemini"`) {
		t.Fatalf("check form does not open its confirmation: %s", form)
	}
	assertDirectSubmission(t, form, "/harnesses/gemini/check")

	dialog := modalDialog(t, body, "confirm-check-gemini")
	for _, want := range []string{
		`class="modal"`, "Check gemini availability?", "uses the gemini account quota",
		"one minimal model request", "no engineering work", "Stopped runs stay stopped.",
		`value="cancel"`, `data-confirm-cancel`, `value="confirm"`, `data-confirm-accept`,
	} {
		if !strings.Contains(dialog, want) {
			t.Errorf("check dialog is missing %q: %s", want, dialog)
		}
	}
}

// Every action form shows a spinner and disables its control until the request
// completes, while keeping the direct POST used without the dashboard script.
func TestEveryActionFormShowsLoadingAndKeepsDirectSubmission(t *testing.T) {
	run := runView{
		Run:                 workflow.Run{ID: "run-1", Repository: "owner/repo", IssueNumber: 7, State: workflow.Manual},
		CanStop:             true,
		CanTakeover:         true,
		CanHandback:         true,
		CanRetry:            true,
		StoppedPublications: []scheduler.StoppedPublication{{Label: "Review report"}},
	}
	body := renderRun(t, run)
	for _, action := range []string{
		"/runs/run-1/takeover", "/runs/run-1/handback", "/runs/run-1/stop", "/runs/run-1/requeue",
	} {
		form := actionForm(t, body, action)
		assertDirectSubmission(t, form, action)
		assertLoading(t, form)
	}

	// Retry's submit control lives in the sticky header, outside its form.
	retryForm := actionForm(t, body, "/runs/run-1/retry")
	assertDirectSubmission(t, retryForm, "/runs/run-1/retry")
	if !strings.Contains(retryForm, `hx-indicator="#retry-submit"`) || !strings.Contains(retryForm, `hx-disabled-elt="#retry-submit"`) {
		t.Errorf("retry form does not drive its header control: %s", retryForm)
	}
	retry := body[strings.Index(body, `id="retry-submit"`):]
	retry = retry[:strings.Index(retry, "</button>")]
	if !strings.Contains(retry, `type="submit"`) || !strings.Contains(retry, "htmx-indicator") {
		t.Errorf("retry control has no loading state: %s", retry)
	}

	dashboard := renderContent(t, pageData{Page: "dashboard", Path: "/", Title: "Dashboard", Token: "csrf-1", Groups: emptyGroups()})
	schedulerForm := actionForm(t, dashboard, "/scheduler/pause")
	assertDirectSubmission(t, schedulerForm, "/scheduler/pause")
	assertLoading(t, schedulerForm)

	checkForm := actionForm(t, renderTemplate(t, "layout", headerData("dashboard")), "/harnesses/gemini/check")
	assertLoading(t, checkForm)
}
