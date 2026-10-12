package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/review"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type ReviewInput struct {
	Issue            ImplementInput
	PRNumber         int
	PRURL            string
	TargetSHA        string
	BaseSHA          string
	Diff             string
	PRBody           string
	PreviousFindings []review.Finding
	FixReport        *review.FixReport
	CI               *ci.Snapshot
	Checks           *workflow.SystemStepSnapshot
	Round            int
}

func WriteReviewInput(ctx context.Context, writer FileWriter, phase PhaseContext, input ReviewInput) (string, error) {
	if phase.Phase != workflow.Review {
		return "", phaseError("phase.unsupported", "Review input requires a review phase", nil)
	}
	if err := validatePhaseContext(phase); err != nil {
		return "", err
	}
	var text strings.Builder
	text.WriteString(fault.HumanActionInstruction + "\n\n")
	if phase.Interruption != "" {
		text.WriteString("## Interruption context\n\n" + phase.Interruption + "\n\n")
	}
	fmt.Fprintf(&text, "# Review issue #%d\n\nRepository: %s\nWorktree: %s\nIssue URL: %s\nPR: #%d %s\nRound: %d\nTarget commit: %s\nDiff range: %s...%s\n\n", input.Issue.IssueNumber, input.Issue.Repository, phase.WorktreePath, input.Issue.IssueURL, input.PRNumber, input.PRURL, input.Round, input.TargetSHA, input.BaseSHA, input.TargetSHA)
	text.WriteString("## Constraints\n\n- Review only the pinned diff and target commit below against the issue.\n- Do not edit repository files, stage changes, commit, reset, switch branches, or push.\n- You may run tests and builds. Ignored test/build output is permitted.\n- Return the complete findings list. Only blocking findings prevent approval.\n- Finding IDs must be unique. Use null for absent locations.\n\n## Output contract\n\nReturn native structured output matching this schema:\n\n```json\n" + review.Schema + "\n```\n\n## Issue\n\n" + input.Issue.IssueTitle + "\n\n" + input.Issue.IssueBody + "\n")
	writeIssueComments(&text, input.Issue.Comments)
	text.WriteString("\n## Pinned diff\n\n")
	text.WriteString(input.Diff)
	text.WriteString("\n\n## Current PR\n\n" + input.PRBody)
	text.WriteString("\n\n## Prior finding outcomes\n\nReturn prior_findings: [] when there are no prior blocking IDs, including the initial review in round 1. Otherwise return exactly one prior_findings entry for each previous blocking finding, including after a changed-head Retry within the same round, with finding_id, outcome (fixed, not_fixed, or no_longer_applies), and a nonempty note. Unknown or duplicate IDs are invalid. A not_fixed finding must appear in findings as blocking with the same ID; fixed and no_longer_applies findings must not remain blocking. A disputed fix response requires an explicit ruling: not_fixed (rejects the dispute) or no_longer_applies (accepts it).\n\nPrior blocking IDs to account for:\n")
	blockingIDs := []string{}
	if len(input.PreviousFindings) > 0 {
		for _, f := range input.PreviousFindings {
			if f.Severity == "blocking" {
				blockingIDs = append(blockingIDs, f.ID)
			}
		}
	}
	ids, _ := json.Marshal(blockingIDs)
	text.WriteString(string(ids) + "\n")
	if input.Round > 1 || len(input.PreviousFindings) > 0 {
		prior, _ := json.MarshalIndent(input.PreviousFindings, "", "  ")
		fix, _ := json.MarshalIndent(input.FixReport, "", "  ")
		text.WriteString("\n\n## Previous findings\n\n" + string(prior) + "\n\n## Implementer fix report\n\n" + string(fix) + "\n\nAdjudicate every dispute independently. Return the complete remaining blocking list for this target, including unresolved earlier findings. Warnings and notes do not prevent approval.\n")
	}
	if input.Checks != nil && input.Checks.TargetSHA == input.TargetSHA {
		results, _ := json.MarshalIndent(input.Checks.Results, "", "  ")
		text.WriteString("\n\n## Local checks\n\nMergeyard ran these configured commands against target commit " + input.Checks.TargetSHA + ":\n\n" + string(results) + "\n")
	}
	if input.CI != nil {
		diagnostics, _ := json.MarshalIndent(input.CI, "", "  ")
		text.WriteString("\n\n## Previous CI repair context\n\n" + string(diagnostics) + "\n\nIndependently review the repair and the complete pinned diff. A successful fix report is not approval; new CI is accepted only after your review.\n")
	}
	if err := writer.WriteFile(ctx, filepath.Join(phase.PhaseDir, "schema.json"), []byte(review.Schema), 0600); err != nil {
		return "", err
	}
	path := filepath.Join(phase.PhaseDir, "input.md")
	if err := writer.WriteFile(ctx, path, []byte(text.String()), 0600); err != nil {
		return "", err
	}
	return path, nil
}
