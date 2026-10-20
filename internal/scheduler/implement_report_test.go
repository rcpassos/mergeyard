package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/workflow"
	"github.com/rcpassos/mergeyard/internal/workspace"
)

// implementReport builds the report an attempt saves when it completes.
func implementReport(status, summary, command string) string {
	return fmt.Sprintf(`{"schema_version":4,"verification":[{"command":%q,"outcome":"passed","excerpt":null}],"criteria":[],"human_action":null,"status":%q,"summary":%q}`, command, status, summary)
}

// implementReportScheduler builds a scheduler over the migrated store the
// runtime owns, with a GitHub boundary that only records publications.
func implementReportScheduler(t *testing.T, db *sql.DB, api GitHub) *Scheduler {
	t.Helper()
	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
	if err != nil {
		t.Fatal(err)
	}
	home, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bus := events.New(db, nil)
	s, err := New(cfg, Resources{DB: db, Events: bus, Workflow: workflow.New(db, bus), Workspace: home}, Dependencies{GitHub: api})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The reviewer's implementer report must come from the attempt publication
// treats as current. A check retry that ends with a valid blocked or failed
// report supersedes the earlier success, so the review input and the PR body
// describe the same evidence instead of mixing two attempts.
func TestCurrentImplementReportUsesSavedEligibility(t *testing.T) {
	for _, tc := range []struct {
		name     string
		attempts []struct {
			status, report string
			eligible       int
		}
		want    string
		wantErr string
	}{
		{
			name: "blocked check retry supersedes the earlier success",
			attempts: []struct {
				status, report string
				eligible       int
			}{
				{"succeeded", implementReport("success", "Added feature", "implement test"), 1},
				{"failed", implementReport("blocked", "Blocked on the API key", "retry test"), 1},
			},
			want: "Blocked on the API key",
		},
		{
			name: "ineligible retry never supersedes the eligible attempt",
			attempts: []struct {
				status, report string
				eligible       int
			}{
				{"succeeded", implementReport("success", "Added feature", "implement test"), 1},
				{"failed", implementReport("failed", "Infrastructure failure", "retry test"), 0},
			},
			want: "Added feature",
		},
		{
			name: "succeeded attempt without a stored report fails closed",
			attempts: []struct {
				status, report string
				eligible       int
			}{
				{"succeeded", "", 0},
			},
			wantErr: "phase.result_missing",
		},
		{
			name: "no eligible attempt leaves the report out",
			attempts: []struct {
				status, report string
				eligible       int
			}{
				{"failed", implementReport("failed", "Infrastructure failure", "retry test"), 0},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db, _ := historyStore(t)
			api := &recordingGitHub{}
			s := implementReportScheduler(t, db, api)
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
				return
			}
			if result == nil || result.Summary != tc.want {
				t.Fatalf("report = %+v, want summary %q", result, tc.want)
			}
			// Publication must treat the same attempt as current.
			if err := s.publishVerification(ctx); err != nil {
				t.Fatal(err)
			}
			if len(api.contents) != 1 || api.contents[0].Summary != tc.want {
				t.Fatalf("published %+v, want one report summarised %q", api.contents, tc.want)
			}
			if len(result.Verification) != 1 || result.Verification[0].Command != api.contents[0].Verification[0].Command {
				t.Fatalf("verification = %+v, want %+v", result.Verification, api.contents[0].Verification)
			}
		})
	}
}
