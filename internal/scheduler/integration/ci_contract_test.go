package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestSchedulerUsesHeadApprovalAndCurrentTestMergeCI(t *testing.T) {

	for _, tc := range []struct {
		MergeState string
		expected   workflow.State
		writes     int
	}{
		{"failure", workflow.Active, 0},
		{"pending", workflow.WaitingForCI, 0},
		{"success", workflow.ReadyToMerge, 1},
	} {
		t.Run(tc.MergeState, func(t *testing.T) {

			_, runtime, api, remote, cfg, runner := testkit.TmuxFlow(t, testkit.SuccessfulScript)
			commands := &testkit.CiContractRunner{Api: api, MergeState: tc.MergeState}
			boundary := testkit.ContractCIGitHub{FakeGitHub: api, Client: github.New(commands)}
			now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), scheduler.Dependencies{GitHub: boundary, Runner: runner, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
			pr := api.Prs["mergeyard/issue-7"]
			yes := true
			pr.Mergeable = &yes
			pr.MergeCommitSHA = "test-merge"
			pr.Base.SHA = testkit.GitCommand(t, remote, "rev-parse", "main")
			if err := s.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			saved, err := runtime.Workflow.Get(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if saved.State != tc.expected || commands.ReadyCalls != tc.writes || (tc.MergeState != "failure" && saved.ApprovedSHA != run.ApprovedSHA) || (tc.MergeState == "failure" && (saved.ApprovedSHA != "" || saved.Phase != workflow.Fix)) || saved.CI.SHA != run.ApprovedSHA || saved.CI.Evidence.MergeSHA != "test-merge" || !saved.CI.Deadline.Equal(run.CI.Deadline) {
				t.Fatalf("head/merge gate: %+v CI=%+v writes=%d", saved, saved.CI, commands.ReadyCalls)
			}
			if len(saved.CI.Evidence.Checks) != 2 || saved.CI.Evidence.Checks[1].SHA != "test-merge" || saved.CI.Evidence.Checks[1].Conclusion != tc.MergeState {
				t.Fatal("test-merge diagnostics lost")
			}
			server, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: s, Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/"+run.ID, nil))
			for _, text := range []string{"Verified test merge commit", "test-merge", run.ApprovedSHA, "https://example.com/" + tc.MergeState} {
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), text) {
					t.Fatalf("missing %q in merge CI detail", text)
				}
			}
		})
	}
}
