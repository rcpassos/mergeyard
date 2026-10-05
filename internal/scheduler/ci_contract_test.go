package scheduler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type ciContractRunner struct {
	api        *fakeGitHub
	mergeState string
	readyCalls int
}

func (r *ciContractRunner) Run(_ context.Context, _ []byte, args ...string) ([]byte, []byte, error) {
	pr := r.api.prs["mergeyard/issue-7"]
	if len(args) > 1 && args[0] == "pr" && args[1] == "ready" {
		r.readyCalls++
		pr.Draft = false
		return nil, nil, nil
	}
	endpoint := ""
	for _, arg := range args {
		if strings.HasPrefix(arg, "repos/") {
			endpoint = arg
			break
		}
	}
	var body any
	switch {
	case strings.Contains(endpoint, "/git/commits/"):
		body = map[string]any{"sha": pr.MergeCommitSHA, "parents": []map[string]string{{"sha": pr.Base.SHA}, {"sha": pr.Head.SHA}}}
	case strings.Contains(endpoint, "/check-runs"):
		body = map[string]any{"total_count": 0, "check_runs": []any{}}
	case strings.Contains(endpoint, "/statuses"):
		state := "success"
		if strings.Contains(endpoint, "/commits/test-merge/") {
			state = r.mergeState
		}
		body = []map[string]string{{"context": "ci", "state": state, "target_url": "https://example.com/" + state}}
	case strings.Contains(endpoint, "/rules/branches/"):
		body = []any{}
	case strings.Contains(endpoint, "/branches/"):
		body = map[string]bool{"protected": false}
	default:
		return nil, nil, fmt.Errorf("unexpected CI fixture endpoint %q", endpoint)
	}
	out, err := json.Marshal(body)
	return out, nil, err
}

type contractCIGitHub struct {
	*fakeGitHub
	client *github.Client
}

func (c contractCIGitHub) PullRequestEvidence(ctx context.Context, repo string, pr github.PullRequest) (ci.Evidence, error) {
	return c.client.PullRequestEvidence(ctx, repo, pr)
}
func (c contractCIGitHub) MarkReady(ctx context.Context, repo string, n int) error {
	return c.client.MarkReady(ctx, repo, n)
}

func TestSchedulerUsesHeadApprovalAndCurrentTestMergeCI(t *testing.T) {
	for _, tc := range []struct {
		mergeState string
		expected   workflow.State
		writes     int
	}{
		{"failure", workflow.Active, 0},
		{"pending", workflow.WaitingForCI, 0},
		{"success", workflow.ReadyToMerge, 1},
	} {
		t.Run(tc.mergeState, func(t *testing.T) {
			_, runtime, api, remote, cfg, runner := localFlow(t, successfulScript)
			commands := &ciContractRunner{api: api, mergeState: tc.mergeState}
			boundary := contractCIGitHub{fakeGitHub: api, client: github.New(commands)}
			now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
			s, err := scheduler.New(cfg, schedulerResources(runtime), scheduler.Dependencies{GitHub: boundary, Runner: runner, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			run := finish(t, s, workflow.WaitingForCI, workflow.Review)
			pr := api.prs["mergeyard/issue-7"]
			yes := true
			pr.Mergeable = &yes
			pr.MergeCommitSHA = "test-merge"
			pr.Base.SHA = gitCommand(t, remote, "rev-parse", "main")
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			saved, err := runtime.Workflow.Get(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if saved.State != tc.expected || commands.readyCalls != tc.writes || (tc.mergeState != "failure" && saved.ApprovedSHA != run.ApprovedSHA) || (tc.mergeState == "failure" && (saved.ApprovedSHA != "" || saved.Phase != workflow.Fix)) || saved.CI.SHA != run.ApprovedSHA || saved.CI.Evidence.MergeSHA != "test-merge" || !saved.CI.Deadline.Equal(run.CI.Deadline) {
				t.Fatalf("head/merge gate: %+v CI=%+v writes=%d", saved, saved.CI, commands.readyCalls)
			}
			if len(saved.CI.Evidence.Checks) != 2 || saved.CI.Evidence.Checks[1].SHA != "test-merge" || saved.CI.Evidence.Checks[1].Conclusion != tc.mergeState {
				t.Fatal("test-merge diagnostics lost")
			}
			server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil))
			for _, text := range []string{"Verified test merge commit", "test-merge", run.ApprovedSHA, "https://example.com/" + tc.mergeState} {
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), text) {
					t.Fatalf("missing %q in merge CI detail", text)
				}
			}
		})
	}
}
