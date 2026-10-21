package web

import (
	"fmt"
	"html/template"
	"time"

	"github.com/rcpassos/mergeyard/internal/workflow"
)

// runStatus is how a run presents itself: a human label, the daisyUI badge
// variant that carries its severity, and for a timed usage limit, its reset.
type runStatus struct {
	Label, Variant string
	Reset          time.Time
}

// statusOf is the single mapping from a run's lifecycle to what the owner
// reads. Templates never print raw state or phase codes.
func statusOf(run workflow.Run, reviewPending bool) runStatus {
	// An observed merge is saved before the owned phase exits, so a merged
	// run can still be ACTIVE until shutdown and cleanup finish.
	if run.Merge != nil {
		return runStatus{Label: "Merged", Variant: "badge-success badge-outline"}
	}
	switch run.State {
	case workflow.Claiming, workflow.Preparing:
		return runStatus{Label: "Starting", Variant: "badge-neutral"}
	case workflow.Active:
		switch {
		case reviewPending:
			return runStatus{Label: "Review queued", Variant: "badge-info"}
		case run.Phase == workflow.Review:
			return runStatus{Label: fmt.Sprintf("Reviewing · round %d", run.ReviewRound), Variant: "badge-info"}
		case run.Phase == workflow.Fix:
			return runStatus{Label: fmt.Sprintf("Fixing · round %d", run.ReviewRound), Variant: "badge-info"}
		}
		return runStatus{Label: "Implementing", Variant: "badge-info"}
	case workflow.WaitingForCI:
		return runStatus{Label: "Waiting for CI", Variant: "badge-info"}
	case workflow.Manual:
		return runStatus{Label: "You're driving", Variant: "badge-accent"}
	case workflow.WaitingForHarness:
		switch wait := run.HarnessWait; {
		case wait == nil:
			return runStatus{Label: "Waiting for harness", Variant: "badge-warning"}
		case wait.Reason == "credits_exhausted":
			return runStatus{Label: "Out of credits", Variant: "badge-warning"}
		default:
			return runStatus{Label: "Usage limited", Variant: "badge-warning", Reset: wait.ResetAt}
		}
	case workflow.NeedsAttention:
		return runStatus{Label: "Needs attention", Variant: "badge-error"}
	case workflow.ReadyToMerge:
		return runStatus{Label: "Ready to merge", Variant: "badge-success"}
	case workflow.Completed:
		return runStatus{Label: "Merged", Variant: "badge-success badge-outline"}
	case workflow.Failed:
		return runStatus{Label: "Failed", Variant: "badge-error badge-outline"}
	case workflow.Stopped:
		return runStatus{Label: "Stopped", Variant: "badge-neutral badge-outline"}
	}
	return runStatus{Label: "Unknown state", Variant: "badge-neutral"}
}

// statusBadge renders a run's status badge. It takes the dashboard's run
// view or a bare persisted run.
func statusBadge(value any) template.HTML {
	var status runStatus
	switch run := value.(type) {
	case runView:
		status = statusOf(run.Run, run.ReviewPending)
	case *runView:
		return statusBadge(*run)
	case workflow.Run:
		status = statusOf(run, false)
	default:
		return ""
	}
	label := template.HTMLEscapeString(status.Label)
	if reset := until(status.Reset); reset != "" {
		label += " · resets " + string(reset)
	}
	return template.HTML(`<span class="badge ` + status.Variant + `">` + label + `</span>`)
}

// stepLabel names the state and phase a retry or handback selected, in the
// words its badge will use. Only an active label names its phase, so other
// states add it.
func stepLabel(state workflow.State, phase workflow.Phase, round int) string {
	run := workflow.Run{State: state, Phase: phase}
	run.ReviewRound = round
	label := statusOf(run, false).Label
	switch {
	case state == workflow.Active || phase == "":
	case phase == workflow.Implement:
		label += " · " + phaseLabel(phase)
	default:
		label += fmt.Sprintf(" · %s, round %d", phaseLabel(phase), round)
	}
	return label
}

var phaseLabels = map[string]string{string(workflow.Implement): "Implementation", string(workflow.Review): "Review", string(workflow.Fix): "Fix"}

// phaseLabel names a phase for reading; it takes a workflow phase or the
// phase string publications record.
func phaseLabel(phase any) string {
	code := fmt.Sprint(phase)
	if label, ok := phaseLabels[code]; ok {
		return label
	}
	return "Not started"
}

// since renders a past instant and until a reset time or deadline. Each is a
// <time> element holding the instant, with an absolute UTC fallback the
// dashboard script replaces with local, relative text. A zero or missing
// time renders nothing.
func since(value any) template.HTML { return timeElement("since", value) }
func until(value any) template.HTML { return timeElement("until", value) }

// storedTimeLayouts are the forms runtime storage writes timestamps in.
var storedTimeLayouts = []string{time.RFC3339Nano, "2006-01-02 15:04:05"}

func timeElement(kind string, value any) template.HTML {
	var at time.Time
	switch v := value.(type) {
	case time.Time:
		at = v
	case string:
		if v == "" {
			return ""
		}
		parsed := false
		for _, layout := range storedTimeLayouts {
			if t, err := time.Parse(layout, v); err == nil {
				at, parsed = t, true
				break
			}
		}
		if !parsed {
			// Not an instant: show it as written rather than drop it.
			return template.HTML(template.HTMLEscapeString(v))
		}
	}
	if at.IsZero() {
		return ""
	}
	at = at.UTC()
	return template.HTML(fmt.Sprintf(`<time datetime="%s" data-time="%s">%s</time>`, at.Format(time.RFC3339), kind, at.Format("2006-01-02 15:04 UTC")))
}
