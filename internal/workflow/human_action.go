package workflow

import (
	"fmt"
	"sort"
	"strings"

	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/review"
)

// attentionAction supplies a decision at the lifecycle boundary, covering every
// Mergeyard-originated attention path, including future adapter error codes.
func attentionAction(run Run, request Request) *fault.HumanAction {
	if action := fault.Action(request.Failure); action != nil {
		return action
	}
	action := &fault.HumanAction{Files: []string{}, Options: []string{}}
	retry := fmt.Sprintf("mergeyard retry %s", run.ID)
	switch request.Failure.Code {
	case "review.max_rounds_exceeded":
		findings := []review.Finding{}
		if run.Review != nil && run.Review.Report != nil {
			findings = run.Review.Report.Findings
		}
		if request.Metadata.Review != nil {
			findings = request.Metadata.Review.Report.Findings
		}
		ids := []string{}
		files := map[string]bool{}
		for _, f := range findings {
			if f.Severity != "blocking" {
				continue
			}
			ids = append(ids, f.ID)
			if f.File != nil {
				files[*f.File] = true
			}
		}
		for file := range files {
			action.Files = append(action.Files, file)
		}
		sort.Strings(action.Files)
		if len(ids) > 0 {
			action.Decision = "Review rounds exhausted; decide how to resolve outstanding blocking findings: " + strings.Join(ids, ", ") + "."
		} else {
			action.Decision = "Review rounds exhausted; repair the failed CI checks or authorize another repair and review round."
		}
		action.Options = []string{"Take over to resolve the remaining work", "Grant another round: " + retry}
	case "git.no_changes":
		action.Decision = "Decide whether the issue is already satisfied or implementation changes are still required. Update the issue with that decision before retrying."
		action.Options = []string{"Close the issue if already satisfied", "Clarify the required changes, then run " + retry}
	case "pr.closed_unmerged":
		action.Decision = "Reopen the unmerged pull request to continue, or stop this run if the work is no longer wanted."
	case "phase.blocked":
		action.Decision = "Answer the agent's request in the saved phase summary: " + request.Failure.Message
	case "phase.result_invalid", "phase.result_missing":
		action.Decision = "Inspect the saved phase output and correct the harness or output contract before running " + retry + "."
	case "checks.failed":
		action.Decision = "Repair the failing configured checks shown in this run, then run " + retry + " to recheck the preserved work."
	case "workspace.setup_failed":
		action.Decision = "Repair the repository setup command using its saved output, then run " + retry + "."
	case "harness.usage_limit_waits_exhausted":
		action.Decision = "Restore harness availability, then run " + retry + " to grant another attempt."
	default:
		switch {
		case strings.Contains(request.Failure.Code, "conflict"):
			action.Decision = "Resolve the Git conflict in the preserved worktree, then run " + retry + "."
		case strings.HasPrefix(request.Failure.Code, "git."):
			action.Decision = "Reconcile the preserved branch and worktree with the reported Git condition: " + request.Failure.Message + ". Then run " + retry + "."
		case strings.HasPrefix(request.Failure.Code, "ci."):
			action.Decision = "Resolve the reported CI condition: " + request.Failure.Message + ". Then run " + retry + "."
		default:
			action.Decision = "Resolve the reported condition (" + request.Failure.Code + "): " + request.Failure.Message + ". Then run " + retry + "."
		}
	}
	return action
}
