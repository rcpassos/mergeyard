package testkit

import (
	"context"
	"testing"

	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type CombinedGitHub struct{ *CiGitHub }

func (f CombinedGitHub) GetPullRequest(ctx context.Context, repo string, n int) (*github.PullRequest, error) {
	return (BranchGitHub{f.FakeGitHub}).GetPullRequest(ctx, repo, n)
}

func CombinedRuns(t *testing.T, s *scheduler.Scheduler) map[int]workflow.Run {
	t.Helper()
	runs, err := s.Runs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[int]workflow.Run, len(runs))
	for _, run := range runs {
		result[run.IssueNumber] = run
	}
	return result
}
