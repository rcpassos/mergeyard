package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/rcpassos/mergeyard/internal/acceptance"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// ImplementInput is the complete issue context for a new or resumed attempt.
// The worktree path comes from PhaseContext, not from issue text.
type ImplementInput struct {
	Repository  string
	IssueNumber int
	IssueTitle  string
	IssueBody   string
	Comments    github.IssueComments
	Guidance    []workflow.HumanGuidance
	IssueURL    string
	BaseBranch  string
	Checks      *workflow.SystemStepSnapshot
	Constraints []string
}

// FileWriter is the file operation supplied by Runner. The caller creates the
// attempt directory before writing input; the adapter does not own its layout.
type FileWriter interface {
	WriteFile(context.Context, string, []byte, fs.FileMode) error
}

// WriteImplementInput writes full context even for resumed sessions, outside
// the worktree. Only its generated path goes into the harness prompt.
func WriteImplementInput(ctx context.Context, writer FileWriter, phase PhaseContext, input ImplementInput) (string, error) {
	if phase.Phase != workflow.Implement {
		return "", phaseError("phase.unsupported", "Implement input requires an implement phase", nil)
	}
	if err := validatePhaseContext(phase); err != nil {
		return "", err
	}
	var text strings.Builder
	text.WriteString(fault.HumanActionInstruction + "\n\n")
	text.WriteString(acceptance.Input(input.IssueBody, false))
	if phase.Interruption != "" {
		text.WriteString("## Interruption context\n\n" + phase.Interruption + "\n\n")
	}
	fmt.Fprintf(&text, "# Implement issue #%d\n\n## Context\n\nRepository: %s\nBase branch: %s\nWorktree: %s\nIssue URL: %s\n\n",
		input.IssueNumber, input.Repository, input.BaseBranch, phase.WorktreePath, input.IssueURL)
	text.WriteString("## Constraints\n\n- Do not commit. Mergeyard owns commits and pull requests.\n")
	for _, constraint := range input.Constraints {
		fmt.Fprintf(&text, "- %s\n", constraint)
	}
	text.WriteString("\n## Output contract\n\nReturn native structured output matching this schema. Use success, blocked, or failed for status.\n\n```json\n")
	text.WriteString(implementSchema)
	text.WriteString("\n```\n\n## Issue\n\n")
	text.WriteString(input.IssueTitle + "\n\n" + input.IssueBody + "\n")
	writeIssueComments(&text, input.Comments)
	writeHumanGuidance(&text, input.Guidance)
	if input.Checks != nil {
		data, err := json.MarshalIndent(input.Checks, "", "  ")
		if err != nil {
			return "", err
		}
		text.WriteString("\n## Local checks\n\nRepair the failing configured command using this diagnostic. Mergeyard will commit and rerun checks before publication.\n\n" + string(data) + "\n")
	}
	if err := writer.WriteFile(ctx, filepath.Join(phase.PhaseDir, "schema.json"), ImplementSchema(), 0600); err != nil {
		return "", err
	}
	if err := writeCriteria(ctx, writer, phase, input.IssueBody); err != nil {
		return "", err
	}
	path := filepath.Join(phase.PhaseDir, "input.md")
	if err := writer.WriteFile(ctx, path, []byte(text.String()), 0600); err != nil {
		return "", err
	}
	return path, nil
}

func validatePhaseContext(ctx PhaseContext) error {
	if ctx.Phase != workflow.Implement && ctx.Phase != workflow.Review && ctx.Phase != workflow.Fix {
		return phaseError("phase.unsupported", "Only implement, review, and fix phases are supported", nil)
	}
	if !filepath.IsAbs(ctx.WorktreePath) || !filepath.IsAbs(ctx.PhaseDir) ||
		strings.ContainsRune(ctx.WorktreePath+ctx.PhaseDir, 0) {
		return phaseError("phase.invalid_request", "Worktree and phase directory paths must be absolute and contain no NUL", nil)
	}
	rel, err := filepath.Rel(ctx.WorktreePath, ctx.PhaseDir)
	if err != nil || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return phaseError("phase.invalid_request", "Phase input must be outside the worktree", err)
	}
	return nil
}

// Guidance stays in files; only the generated input path enters harness commands.
func writeHumanGuidance(text *strings.Builder, notes []workflow.HumanGuidance) {
	if len(notes) == 0 {
		return
	}
	text.WriteString("\n## Human guidance\n\nThese notes were submitted by the user on Retry and apply for the rest of this run, in submission order.\n\n")
	for _, note := range notes {
		fmt.Fprintf(text, "### %s — %s\n\n%s\n\n", note.Author, note.CreatedAt, note.Text)
	}
}
