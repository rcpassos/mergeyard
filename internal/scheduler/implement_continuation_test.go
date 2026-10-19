package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// Continuation is the recovery path after the control plane stopped between
// saving an attempt's report and advancing the run. The attempt report is the
// durable source, so a cleaned phase directory cannot change the decision.
func TestSuccessfulImplementContinuationUsesSavedReport(t *testing.T) {
	ctx := context.Background()
	db, _ := historyStore(t)
	s := historyScheduler(t, db)
	run := seedImplementContinuation(t, s, "succeeded", "", `{"schema_version":4,"criteria":[],"human_action":null,"status":"success","summary":"Saved implementation","verification":[{"command":"go test ./...","outcome":"passed","excerpt":null}]}`)

	result, done, err := s.implement(ctx, s.cfg.Repositories[0], run, github.Issue{}, managedgit.Run{})
	if err != nil || !done {
		t.Fatalf("continuation = %+v done=%t err=%v", result, done, err)
	}
	if result.SchemaVersion != 4 || result.Summary != "Saved implementation" || len(result.Verification) != 1 || result.Verification[0].Command != "go test ./..." {
		t.Fatalf("continuation did not return the saved report: %+v", result)
	}
	var attempts int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM phase_attempts WHERE run_id=? AND phase='implement'`, run.ID).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatalf("continuation launched another attempt: %d %v", attempts, err)
	}
}

func TestBlockedImplementContinuationUsesSavedReport(t *testing.T) {
	ctx := context.Background()
	db, _ := historyStore(t)
	s := historyScheduler(t, db)
	run := seedImplementContinuation(t, s, "failed", "phase.blocked: Need a decision", `{"schema_version":4,"criteria":[],"human_action":{"decision":"Choose behavior","files":["feature.txt"],"options":["Keep","Replace"]},"status":"blocked","summary":"Need a decision","verification":[]}`)

	_, done, err := s.implement(ctx, s.cfg.Repositories[0], run, github.Issue{}, managedgit.Run{})
	if done || err == nil {
		t.Fatalf("blocked continuation = done=%t err=%v", done, err)
	}
	var coded *fault.Error
	if !errors.As(err, &coded) || coded.Code != "phase.blocked" {
		t.Fatalf("blocked continuation lost its cause: %v", err)
	}
	want := &fault.HumanAction{Decision: "Choose behavior", Files: []string{"feature.txt"}, Options: []string{"Keep", "Replace"}}
	if !reflect.DeepEqual(coded.HumanAction, want) {
		t.Fatalf("blocked continuation action = %+v, want %+v", coded.HumanAction, want)
	}
	var attempts int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM phase_attempts WHERE run_id=? AND phase='implement'`, run.ID).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatalf("blocked continuation launched another attempt: %d %v", attempts, err)
	}
}

// seedImplementContinuation records a finished attempt outside a reachable
// phase directory: the input file exists, but the result file was cleaned.
func seedImplementContinuation(t *testing.T, s *Scheduler, status, failure, report string) workflow.Run {
	t.Helper()
	ctx := context.Background()
	phaseDir := filepath.Join(t.TempDir(), "implement-0-1")
	if err := os.MkdirAll(phaseDir, 0700); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(phaseDir, "input.md")
	if err := os.WriteFile(input, []byte("input"), 0600); err != nil {
		t.Fatal(err)
	}
	result := filepath.Join(phaseDir, "result.json")
	if _, err := os.Stat(result); !os.IsNotExist(err) {
		t.Fatalf("fixture result file exists: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO runs(id,repository,issue_number,state,current_phase,implementer_session_id) VALUES('continuation','owner/repo',7,'ACTIVE','implement','session')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status,step,input_path,result_path,report_json,report_eligible,error) VALUES('continuation-attempt','continuation','implement','implementer',0,1,'claude',?,'',?,?,?,1,?)`, status, input, result, report, failure); err != nil {
		t.Fatal(err)
	}
	return workflow.Run{ID: "continuation", Repository: "owner/repo", IssueNumber: 7, State: workflow.Active, Phase: workflow.Implement}
}
