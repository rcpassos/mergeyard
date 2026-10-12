package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func (s *Scheduler) phaseIssueInput(ctx context.Context, repo config.Repository, runID string, issue github.Issue, baseBranch string, phase *harness.PhaseContext) (harness.ImplementInput, error) {
	input := harness.ImplementInput{Repository: repo.Repo, IssueNumber: issue.Number, IssueTitle: issue.Title, IssueBody: issue.Body, IssueURL: issue.URL, BaseBranch: baseBranch}
	retries, err := workflow.LoadRetries(ctx, s.db, runID)
	if err != nil {
		return input, err
	}
	for _, retry := range retries {
		if retry.Guidance != nil {
			input.Guidance = append(input.Guidance, *retry.Guidance)
		}
	}
	comments, err := s.deps.GitHub.ListIssueComments(ctx, repo.Repo, issue.Number, repo.TrustedLabelers)
	if err != nil {
		return input, err
	}
	input.Comments = comments
	// Compare identities with the last input for this role, including earlier
	// rounds/phases. GitHub creation timestamps have only second precision and
	// cannot reliably tell which comments arrived after a previous input fetch.
	var previousInput string
	err = s.db.QueryRowContext(ctx, `SELECT input_path FROM phase_attempts
 WHERE run_id=? AND role=? AND step='' ORDER BY started_at DESC,rowid DESC LIMIT 1`, runID, phase.Phase.Role()).Scan(&previousInput)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return input, err
	}
	previousIDs := map[int64]bool{}
	haveSnapshot := false
	if previousInput != "" {
		data, err := s.deps.Runner.ReadFile(ctx, filepath.Join(filepath.Dir(previousInput), "issue-comments.json"))
		if errors.Is(err, fs.ErrNotExist) {
			// Attempts predating comment inputs have no snapshot to compare.
			phase.Interruption = strings.TrimSpace(phase.Interruption + "\nIssue comments were refreshed for this attempt. Read ## Issue comments for newly available clarifications before continuing.")
		} else if err != nil {
			return input, err
		} else {
			var ids []int64
			if err := json.Unmarshal(data, &ids); err != nil {
				return input, err
			}
			haveSnapshot = true
			for _, id := range ids {
				previousIDs[id] = true
			}
		}
	}
	added := 0
	ids := make([]int64, 0, len(comments.Comments))
	for _, comment := range comments.Comments {
		ids = append(ids, comment.ID)
		if !previousIDs[comment.ID] {
			added++
		}
	}
	if haveSnapshot && added > 0 {
		phase.Interruption = strings.TrimSpace(phase.Interruption + fmt.Sprintf("\nNew trusted issue comments since the previous input: %d. Read ## Issue comments for the refreshed clarifications (subject to the size cap) before continuing.", added))
	}
	data, err := json.Marshal(ids)
	if err != nil {
		return input, err
	}
	return input, s.deps.Runner.WriteFile(ctx, filepath.Join(phase.PhaseDir, "issue-comments.json"), data, 0600)
}
