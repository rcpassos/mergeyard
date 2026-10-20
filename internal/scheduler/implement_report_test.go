package scheduler

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/rcpassos/mergeyard/internal/fault"
)

// implementAttempt is one saved implement attempt in the eligibility fixtures.
type implementAttempt struct {
	status   string
	report   string
	eligible int
}

// implementReportJSON builds the report an attempt saves when it completes.
// An empty command leaves out the verification evidence that only the current
// report schema carries.
func implementReportJSON(status, summary, command string) string {
	verification := ""
	if command != "" {
		verification = fmt.Sprintf(`"verification":[{"command":%q,"outcome":"passed","excerpt":null}],`, command)
	}
	return fmt.Sprintf(`{"schema_version":4,%s"criteria":[],"human_action":null,"status":%q,"summary":%q}`, verification, status, summary)
}

// The reviewer's implementer report must come from the attempt publication
// treats as current. A check retry that ends with a valid blocked or failed
// report supersedes the earlier success, so the review input and the PR body
// describe the same evidence instead of mixing two attempts.
func TestCurrentImplementReportUsesSavedEligibility(t *testing.T) {
	for _, tc := range []struct {
		name      string
		attempts  []implementAttempt
		want      string
		published string
		wantErr   string
	}{
		{
			name: "blocked check retry supersedes the earlier success",
			attempts: []implementAttempt{
				{"succeeded", implementReportJSON("success", "Added feature", "implement test"), 1},
				{"failed", implementReportJSON("blocked", "Blocked on the API key", "retry test"), 1},
			},
			want:      "Blocked on the API key",
			published: "Blocked on the API key",
		},
		{
			name: "ineligible retry never supersedes the eligible attempt",
			attempts: []implementAttempt{
				{"succeeded", implementReportJSON("success", "Added feature", "implement test"), 1},
				{"failed", implementReportJSON("failed", "Infrastructure failure", "retry test"), 0},
			},
			want:      "Added feature",
			published: "Added feature",
		},
		{
			name: "a report without verification evidence never displaces the published one",
			attempts: []implementAttempt{
				{"succeeded", implementReportJSON("success", "Added feature", "implement test"), 1},
				{"failed", implementReportJSON("blocked", "Blocked on the API key", ""), 1},
			},
			want:      "Added feature",
			published: "Added feature",
		},
		{
			name: "an eligible report without verification evidence is still the implementer's account",
			attempts: []implementAttempt{
				{"succeeded", implementReportJSON("success", "Added feature", ""), 1},
			},
			want: "Added feature",
		},
		{
			name: "succeeded attempt without a stored report fails closed",
			attempts: []implementAttempt{
				{"succeeded", "", 0},
			},
			wantErr: "phase.result_missing",
		},
		{
			name: "no eligible attempt leaves the report out",
			attempts: []implementAttempt{
				{"failed", implementReportJSON("failed", "Infrastructure failure", "retry test"), 0},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db, _ := historyStore(t)
			api := &recordingGitHub{}
			s := eligibilityScheduler(t, db, api)
			if _, err := db.ExecContext(ctx, `
INSERT INTO runs(id,repository,issue_number,state,current_phase,review_round,pr_number) VALUES('eligibility','owner/repo',7,'NEEDS_ATTENTION','implement',0,101);
INSERT INTO scheduler_runs(run_id,issue_json) VALUES('eligibility','{"number":7,"title":"Feature"}');`); err != nil {
				t.Fatal(err)
			}
			for i, attempt := range tc.attempts {
				var report any
				if attempt.report != "" {
					report = attempt.report
				}
				if _, err := db.ExecContext(ctx, `INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status,step,report_json,report_eligible,error) VALUES(?,?,'implement','implementer',0,?,'claude',?,'',?,?,'')`, fmt.Sprintf("attempt-%d", i+1), "eligibility", i+1, attempt.status, report, attempt.eligible); err != nil {
					t.Fatal(err)
				}
			}
			result, err := s.currentImplementReport(ctx, "eligibility")
			if tc.wantErr != "" {
				var coded *fault.Error
				if !errors.As(err, &coded) || coded.Code != tc.wantErr {
					t.Fatalf("error = %v, want %s", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if result != nil {
					t.Fatalf("report = %+v, want none", result)
				}
			} else if result == nil || result.Summary != tc.want {
				t.Fatalf("report = %+v, want summary %q", result, tc.want)
			}
			// Publication must treat the same attempt as current.
			if err := s.publishVerification(ctx); err != nil {
				t.Fatal(err)
			}
			if tc.published == "" {
				if len(api.contents) != 0 {
					t.Fatalf("published %+v, want nothing", api.contents)
				}
				return
			}
			if len(api.contents) != 1 || api.contents[0].Summary != tc.published {
				t.Fatalf("published %+v, want one report summarised %q", api.contents, tc.published)
			}
			if result == nil || len(result.Verification) != len(api.contents[0].Verification) {
				t.Fatalf("verification = %+v, want %+v", result.Verification, api.contents[0].Verification)
			}
		})
	}
}
