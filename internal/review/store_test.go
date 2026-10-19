package review_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/rcpassos/mergeyard/internal/review"
	"github.com/rcpassos/mergeyard/internal/store"
)

// Loading a saved review applies the version's contract: versions without
// prior findings present an empty list, and newer versions keep what they saved.
func TestStoredReviewPriorFindingsByVersion(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for i, tc := range []struct {
		name   string
		report string
		want   int
	}{
		{"v1 normalizes an absent list", `{"schema_version":1,"status":"approved","summary":"Clean","findings":[]}`, 0},
		{"v4 keeps prior findings", `{"schema_version":4,"status":"approved","summary":"Clean","findings":[],"prior_findings":[{"finding_id":"F1","outcome":"fixed","note":"Repaired"}],"human_action":null,"criteria":[]}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runID, attemptID := tc.name, tc.name+"-attempt"
			if _, err := db.ExecContext(ctx, `INSERT INTO runs (id,repository,issue_number,state) VALUES (?,?,?,?)`, runID, "owner/repo", i+1, "ACTIVE"); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO phase_attempts (id,run_id,phase,role,round,attempt,agent,status) VALUES (?,?,'review','reviewer',1,1,'claude','succeeded')`, attemptID, runID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO review_attempts (attempt_id,target_sha,diff,snapshot_json,session_id,permission_mode,allowed_tools_json,restored,contaminated,accepted,report_json) VALUES (?,'sha','','','session','bypassPermissions','[]',1,0,1,?)`, attemptID, tc.report); err != nil {
				t.Fatal(err)
			}
			snapshot, err := review.LoadSnapshot(ctx, db, runID)
			if err != nil || snapshot == nil || snapshot.Report == nil {
				t.Fatalf("snapshot = %+v, err = %v", snapshot, err)
			}
			if snapshot.Report.PriorFindings == nil || len(snapshot.Report.PriorFindings) != tc.want {
				t.Fatalf("prior findings = %+v, want %d entries", snapshot.Report.PriorFindings, tc.want)
			}
		})
	}
}

// Fix snapshots expose the eligibility frozen with the report so review input
// can use the same rule as publication.
func TestFixHistoryExposesSavedEligibility(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	const report = `{"schema_version":4,"status":"success","summary":"Fixed","responses":[],"verification":[],"criteria":[],"human_action":null}`
	if _, err := db.ExecContext(ctx, `
INSERT INTO runs (id,repository,issue_number,state) VALUES ('eligibility','owner/repo',7,'ACTIVE');
INSERT INTO phase_attempts (id,run_id,phase,role,round,attempt,agent,status,step,report_json,report_eligible) VALUES
 ('eligible','eligibility','fix','implementer',1,1,'claude','succeeded','',?,1),
 ('ineligible','eligibility','fix','implementer',1,2,'claude','failed','',?,0);
INSERT INTO fix_attempts (attempt_id,session_id,target_sha,findings_json,permission_mode,allowed_tools_json,report_json) VALUES
 ('eligible','session','sha','[]','default','[]',?),
 ('ineligible','session','sha','[]','default','[]',?);`, report, report, report, report); err != nil {
		t.Fatal(err)
	}
	history, err := review.LoadFixHistory(ctx, db, "eligibility")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || !history[0].ReportEligible || history[1].ReportEligible {
		t.Fatalf("fix eligibility = %+v", history)
	}
	if !(history[0].Sequence < history[1].Sequence) {
		t.Fatalf("fix attempt order lost: %+v", history)
	}
}
