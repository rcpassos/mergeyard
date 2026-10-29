package testkit

import (
	"context"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/app"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

const ChangesReview = `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":4,"criteria":[],"human_action":null,"prior_findings":[],"status":"changes_required","summary":"Fix bug","findings":[{"id":"F1","severity":"blocking","title":"Bug","details":"Broken","file":"feature.txt","line":1},{"id":"F2","severity":"blocking","title":"Disputable","details":"Explain","file":null,"line":null}]}}'`
const FixedReport = `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":4,"verification":[],"criteria":[],"human_action":null,"status":"success","summary":"Fixed <script>bug</script>","responses":[{"finding_id":"F1","resolution":"fixed","note":"Changed source"},{"finding_id":"F2","resolution":"disputed","note":"Already safe"}]}}'`
const DisputedReport = `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":4,"verification":[],"criteria":[],"human_action":null,"status":"success","summary":"Disputed","responses":[{"finding_id":"F1","resolution":"disputed","note":"Safe"},{"finding_id":"F2","resolution":"disputed","note":"Already safe"}]}}'`

var ApprovedAfterFixReview = strings.Replace(ApprovedReview, `"prior_findings":[]`, `"prior_findings":[{"finding_id":"F1","outcome":"fixed","note":"Source repaired"},{"finding_id":"F2","outcome":"no_longer_applies","note":"Dispute accepted"}]`, 1)
var ApprovedAfterDisputesReview = strings.Replace(ApprovedAfterFixReview, `"outcome":"fixed"`, `"outcome":"no_longer_applies"`, 1)

func LoopScript(fix string) string {
	approved := ApprovedAfterDisputesReview
	if strings.Contains(fix, `"resolution":"fixed"`) {
		approved = ApprovedAfterFixReview
	}

	return `case "$*" in
 *review-1-*) ` + ChangesReview + `;;
 *review-*) ` + approved + `;;
 *fix-*) ` + fix + `;;
 *) ` + SuccessfulScript + `;;
 esac`
}

// GitHub observes the real managed branch, including pushes after PR creation.
type BranchGitHub struct{ *FakeGitHub }

func (f BranchGitHub) GetPullRequest(ctx context.Context, repo string, n int) (*github.PullRequest, error) {
	pr, err := f.FakeGitHub.GetPullRequest(ctx, repo, n)
	if err == nil && pr != nil {
		pr.Head.SHA = f.Head(pr.Head.Ref)
	}
	return pr, err
}

func AdvanceFixUntil(t *testing.T, s *scheduler.Scheduler, runtime *app.Runtime, id string, predicate func(workflow.Run) bool) workflow.Run {
	t.Helper()
	var current workflow.Run
	WaitFor(t, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		var err error
		current, err = runtime.Workflow.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		return predicate(current)
	})
	return current
}

type HeldFixPush struct {
	*managedgit.Manager
	Entered, Release chan struct{}
}

func (g HeldFixPush) PushFix(ctx context.Context, run managedgit.Run, previous, target string) error {
	close(g.Entered)
	select {
	case <-g.Release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return g.Manager.PushFix(ctx, run, previous, target)
}
