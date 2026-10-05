package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/review"
	"github.com/rcpassos/mergeyard/internal/workflow"
	"path/filepath"
	"strings"
)

type FixInput struct {
	Issue     ImplementInput
	PRNumber  int
	PRURL     string
	PRBody    string
	TargetSHA string
	Round     int
	Findings  []review.Finding
	CI        *ci.Snapshot
}

func WriteFixInput(ctx context.Context, writer FileWriter, phase PhaseContext, input FixInput) (string, error) {
	if phase.Phase != workflow.Fix {
		return "", phaseError("phase.unsupported", "Fix input requires fix phase", nil)
	}
	if err := validatePhaseContext(phase); err != nil {
		return "", err
	}
	var text strings.Builder
	fmt.Fprintf(&text, "# Fix issue #%d\n\nRepository: %s\nBase branch: %s\nWorktree: %s\nIssue URL: %s\nPR: #%d %s\nRound: %d\nCurrent commit: %s\n\n", input.Issue.IssueNumber, input.Issue.Repository, input.Issue.BaseBranch, phase.WorktreePath, input.Issue.IssueURL, input.PRNumber, input.PRURL, input.Round, input.TargetSHA)
	text.WriteString("## Constraints\n\n- Fix the supplied blocking findings, or dispute each with a concrete explanation.\n- Respond exactly once per supplied finding, with fixed or disputed and a nonempty note.\n- A success report must account for every finding. Blocked/failed reports may describe partial work.\n- Do not commit or push. Mergeyard owns publication.\n- The independent reviewer adjudicates disputes. Your success does not approve the PR.\n\n## Output contract\n\nReturn native structured output matching this schema:\n\n```json\n" + review.FixSchema + "\n```\n\n## Issue\n\n" + input.Issue.IssueTitle + "\n\n" + input.Issue.IssueBody + "\n\n## Current PR\n\n" + input.PRBody + "\n\n## Blocking findings\n\n")
	findings, err := json.MarshalIndent(input.Findings, "", "  ")
	if err != nil {
		return "", err
	}
	text.Write(findings)
	if input.CI != nil {
		diagnostics, err := json.MarshalIndent(input.CI, "", "  ")
		if err != nil {
			return "", err
		}
		text.WriteString("\n\n## CI repair\n\nRepair cause: " + input.CI.RepairCause + "\n\n" + string(diagnostics) + "\n\nFix the code causing the supplied failed or terminal timed-out checks. A terminal check timeout differs from Mergeyard's wait deadline. Do not rerun checks or speculate about canceled/action-required checks. For CI-only fixes return responses: []; do not invent findings. Summarize the changes and local validation. Another independent review is required before new CI can authorize readiness.\n")
	}
	if err := writer.WriteFile(ctx, filepath.Join(phase.PhaseDir, "schema.json"), []byte(review.FixSchema), 0600); err != nil {
		return "", err
	}
	path := filepath.Join(phase.PhaseDir, "input.md")
	return path, writer.WriteFile(ctx, path, []byte(text.String()), 0600)
}
