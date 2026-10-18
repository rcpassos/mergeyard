package scheduler

import (
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/workflow"
	"github.com/rcpassos/mergeyard/internal/workspace"
)

// A fix rejected after it was saved must stay the publication intent: the
// rejection rewrites the attempt's error and status, while the frozen
// eligibility and the saved report survive. Without that, publication would
// fall back to an older attempt and could overwrite the newer evidence.
func TestRejectedFixKeepsItsPublicationIntent(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		eligible   int
	}{
		{"rejected fix keeps frozen eligibility", "Fixed the finding", 1},
		{"fix without a valid completion never wins", "Implemented the feature", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := historyStore(t)
			ctx := t.Context()
			if _, err := db.ExecContext(ctx, `
INSERT INTO runs(id,repository,issue_number,state,current_phase,review_round,pr_number) VALUES('eligibility','owner/repo',7,'NEEDS_ATTENTION','fix',1,101);
INSERT INTO scheduler_runs(run_id,issue_json) VALUES('eligibility','{"number":7,"title":"Feature"}');
INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status,step,report_json,report_eligible,error) VALUES
 ('implement-attempt','eligibility','implement','implementer',0,1,'claude','succeeded','','{"schema_version":4,"human_action":null,"status":"success","summary":"Implemented the feature","verification":[{"command":"implement test","outcome":"passed","excerpt":null}]}',1,''),
 ('fix-attempt','eligibility','fix','implementer',1,1,'claude','failed','',NULL,?,'phase.result_invalid: Fix claims a fixed finding but made no code changes');
INSERT INTO fix_attempts(attempt_id,session_id,target_sha,findings_json,permission_mode,allowed_tools_json,report_json) VALUES
 ('fix-attempt','session','target','[]','default','[]','{"schema_version":4,"status":"success","summary":"Fixed the finding","responses":[],"verification":[{"command":"fix test","outcome":"passed","excerpt":null}]}');`, tc.eligible); err != nil {
				t.Fatal(err)
			}
			cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
			if err != nil {
				t.Fatal(err)
			}
			home, err := workspace.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			bus := events.New(db, nil)
			api := &recordingGitHub{}
			s, err := New(cfg, Resources{DB: db, Events: bus, Workflow: workflow.New(db, bus), Workspace: home}, Dependencies{GitHub: api})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.publishVerification(ctx); err != nil {
				t.Fatal(err)
			}
			if len(api.contents) != 1 || api.contents[0].Summary != tc.want {
				t.Fatalf("published %+v, want one report summarised %q", api.contents, tc.want)
			}
			wantSource := "implement-attempt"
			if tc.eligible == 1 {
				wantSource = "fix-attempt"
			}
			var source string
			if err := db.QueryRowContext(ctx, `SELECT json_extract(payload_json,'$.source_attempt') FROM events WHERE run_id='eligibility' AND type='verification.published'`).Scan(&source); err != nil {
				t.Fatal(err)
			}
			if source != wantSource {
				t.Fatalf("receipt = %q, want %q", source, wantSource)
			}
		})
	}
}
