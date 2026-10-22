package web

import (
	"fmt"
	"html/template"

	"github.com/rcpassos/mergeyard/internal/workflow"
)

// lifecycleStep is one stop on the run detail stepper. Done marks the steps the
// run has passed, and Current marks where it is now.
type lifecycleStep struct {
	Label         string
	Done, Current bool
}

var lifecycleNames = []string{"Claimed", "Implement", "Review", "Fix", "CI", "Ready", "Merged"}

// lifecycleSteps maps a run to the Claimed → Implement → Review → Fix → CI →
// Ready → Merged life. A manual takeover or a harness wait stays on the phase
// it interrupted, and an observed merge wins even before the owned phase exits.
func lifecycleSteps(view *runView) []lifecycleStep {
	if view == nil {
		return nil
	}
	run := view.Run
	current := lifecycleIndex(run)
	steps := make([]lifecycleStep, len(lifecycleNames))
	for i, name := range lifecycleNames {
		label := name
		if i == 2 && run.ReviewRound > 0 {
			label = fmt.Sprintf("Review · round %d", run.ReviewRound)
		}
		steps[i] = lifecycleStep{Label: label, Done: i <= current, Current: i == current}
	}
	return steps
}

func lifecycleIndex(run workflow.Run) int {
	if run.Merge != nil || run.State == workflow.Completed {
		return 6
	}
	switch run.State {
	case workflow.ReadyToMerge:
		return 5
	case workflow.WaitingForCI:
		return 4
	case workflow.Claiming, workflow.Preparing:
		return 0
	}
	switch run.Phase {
	case workflow.Implement:
		return 1
	case workflow.Review:
		return 2
	case workflow.Fix:
		return 3
	}
	return 0
}

// shortCopy renders a shortened value as a copy control carrying the full
// value. SHAs and session IDs are unreadable in full but must stay exact.
func shortCopy(value string, length int, label string) template.HTML {
	if value == "" {
		return ""
	}
	if length <= 0 {
		length = 7
	}
	runes := []rune(value)
	visible := value
	if len(runes) > length {
		visible = string(runes[:length]) + "…"
	}
	full := template.HTMLEscapeString(value)
	return template.HTML(`<button type="button" class="copy-value" data-copy="` + full +
		`" title="` + full + `" aria-label="Copy ` + template.HTMLEscapeString(label) + `: ` + full +
		`">` + template.HTMLEscapeString(visible) + `</button>`)
}

// isLatest reports whether index is the last item in a list of length items.
// The latest entry of each history kind is open by default and older entries
// are collapsed.
func isLatest(index, length int) bool {
	return index == length-1
}

// currentMatters reports whether the current area has anything to show. Keep
// this beside the banners it guards so a new one cannot sit beside an empty
// state.
func currentMatters(run *runView) bool {
	if run == nil {
		return false
	}
	return run.Merge != nil || run.State == workflow.ReadyToMerge || run.ReviewPending ||
		run.Stopping || run.TakeoverPrerequisite != "" || run.TakeoverStatus == workflow.TakeoverRequested ||
		run.ResumeCommand != "" || run.HumanAction != nil || run.LastErrorCode != "" ||
		len(run.StoppedPublications) > 0 || (run.State == workflow.WaitingForHarness && run.HarnessWait != nil) ||
		run.CanRetry || run.CanHandback
}
