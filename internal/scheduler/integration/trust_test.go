package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestAllPhasesUseAuthorizedIssueSnapshot(t *testing.T) {
	t.Parallel()

	checkInput := `phase_dir=''
previous=''
for arg do
 if [ "$previous" = '--add-dir' ]; then phase_dir=$arg; fi
 previous=$arg
 done
if ! /usr/bin/grep -Fq 'APPROVED-ISSUE-SNAPSHOT' "$phase_dir/input.md" ||
   /usr/bin/grep -Fq 'UNTRUSTED-BODY-EDIT' "$phase_dir/input.md"; then
 printf 'unauthorized issue snapshot reached harness\n' >&2
 exit 9
fi
`
	_, rt, api, _, cfg, r := testkit.TmuxFlow(t, checkInput+testkit.LoopScript("printf repaired > feature.txt\n"+testkit.FixedReport))
	api.Issues["owner/repo"][0].Body = "APPROVED-ISSUE-SNAPSHOT"
	reads := 0
	api.GetIssueHook = func(repo string, n int) (github.Issue, error) {
		reads++
		issue := api.Issues[repo][0]
		if reads > 1 {
			issue.Body = "UNTRUSTED-BODY-EDIT"
		}
		return issue, nil
	}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: testkit.BranchGitHub{FakeGitHub: api}, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	var run workflow.Run
	testkit.WaitForWithin(t, 45*time.Second, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != 1 {
			return false
		}
		run = runs[0]
		return run.State == workflow.WaitingForCI || run.State == workflow.NeedsAttention
	})
	if run.State != workflow.WaitingForCI || run.ReviewRound != 2 {
		t.Fatalf("unchecked refreshed body reached implement/review/fix: %+v", run)
	}
}
