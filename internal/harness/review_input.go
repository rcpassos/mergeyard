package harness

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rcpassos/mergeyard/internal/review"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type ReviewInput struct {
	Issue     ImplementInput
	PRNumber  int
	PRURL     string
	TargetSHA string
	BaseSHA   string
	Diff      string
	Round     int
}

func WriteReviewInput(ctx context.Context, writer FileWriter, phase PhaseContext, input ReviewInput) (string, error) {
	if phase.Phase != workflow.Review {
		return "", phaseError("phase.unsupported", "Review input requires a review phase", nil)
	}
	if err := validatePhaseContext(phase); err != nil {
		return "", err
	}
	var text strings.Builder
	fmt.Fprintf(&text, "# Review issue #%d\n\nRepository: %s\nWorktree: %s\nIssue URL: %s\nPR: #%d %s\nRound: %d\nTarget commit: %s\nDiff range: %s...%s\n\n", input.Issue.IssueNumber, input.Issue.Repository, phase.WorktreePath, input.Issue.IssueURL, input.PRNumber, input.PRURL, input.Round, input.TargetSHA, input.BaseSHA, input.TargetSHA)
	text.WriteString("## Constraints\n\n- Review only the pinned diff and target commit below against the issue.\n- Do not edit repository files, stage changes, commit, reset, switch branches, or push.\n- You may run tests and builds. Ignored test/build output is permitted.\n- Return the complete findings list. Only blocking findings prevent approval.\n- Finding IDs must be unique. Use null for absent locations.\n\n## Output contract\n\nReturn native structured output matching this schema:\n\n```json\n" + review.Schema + "\n```\n\n## Issue\n\n" + input.Issue.IssueTitle + "\n\n" + input.Issue.IssueBody + "\n\n## Pinned diff\n\n")
	text.WriteString(input.Diff)
	path := filepath.Join(phase.PhaseDir, "input.md")
	if err := writer.WriteFile(ctx, path, []byte(text.String()), 0600); err != nil {
		return "", err
	}
	return path, nil
}
