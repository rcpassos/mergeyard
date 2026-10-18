package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/maintenance"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// A tick observes every run that still owes durable work and no finished run
// without it. Dropping one here would strand its pending retry, uncleaned base
// scratch checkout or merge cleanup forever.
func TestTickRunsKeepsPendingWorkAndSkipsFinishedHistory(t *testing.T) {
	ctx := context.Background()
	db, _ := historyStore(t)
	s := historyScheduler(t, db)

	answered, pending := new(bool), new(bool)
	*pending = true

	scratch := func(cleaned bool) string {
		data, err := json.Marshal(ci.Snapshot{Base: &ci.BaseVerification{ScratchPath: "/scratch", Cleaned: cleaned}})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	merge := func(processExited bool) string {
		data, err := json.Marshal(maintenance.Snapshot{
			ObservedAt: "2026-01-01T00:00:00Z", ProcessExited: processExited,
			ReadyRemoved: true, RunningRemoved: true, AttentionRemoved: true, IssueClosed: true,
			ReviewRestored: true, WorktreeStarted: true, WorktreeRemoved: true, BranchDeleted: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	cases := []struct {
		name, state string
		retry       *bool
		scratch     string
		merge       string
		want        bool
	}{
		{"active", string(workflow.Active), nil, "", "", true},
		{"manual", string(workflow.Manual), nil, "", "", true},
		{"ready to merge", string(workflow.ReadyToMerge), nil, "", "", true},
		{"finished", string(workflow.Completed), nil, "", "", false},
		{"failed", string(workflow.Failed), nil, "", "", false},
		{"stopped", string(workflow.Stopped), nil, "", "", false},
		{"answered retry", string(workflow.Completed), answered, "", "", false},
		{"pending retry", string(workflow.Completed), pending, "", "", true},
		{"cleaned base scratch", string(workflow.Completed), nil, scratch(true), "", false},
		{"uncleaned base scratch", string(workflow.Completed), nil, scratch(false), "", true},
		{"pending merge cleanup", string(workflow.Completed), nil, "", merge(false), true},
		{"finished merge cleanup", string(workflow.Completed), nil, "", merge(true), false},
		// Dispatch capacity holds an issue while any merge cleanup is pending,
		// even after the run itself reached a terminal state.
		{"failed run with pending merge cleanup", string(workflow.Failed), nil, "", merge(false), true},
	}
	want := map[string]bool{}
	for i, tc := range cases {
		id := fmt.Sprintf("tick-run-%02d-%s", i, tc.name)
		want[id] = tc.want
		if _, err := db.ExecContext(ctx, `INSERT INTO runs (id,repository,issue_number,state,current_phase,review_round) VALUES (?,?,?,?,'implement',0)`, id, historyRepo, 10000+i, tc.state); err != nil {
			t.Fatal(err)
		}
		if tc.retry != nil {
			if _, err := db.ExecContext(ctx, `INSERT INTO run_retries (run_id,pending,snapshot_json) VALUES (?,?,'{}')`, id, *tc.retry); err != nil {
				t.Fatal(err)
			}
		}
		if tc.scratch != "" {
			if _, err := db.ExecContext(ctx, `INSERT INTO ci_waits (run_id,snapshot_json) VALUES (?,?)`, id, tc.scratch); err != nil {
				t.Fatal(err)
			}
		}
		if tc.merge != "" {
			if _, err := db.ExecContext(ctx, `INSERT INTO merge_cleanup (run_id,snapshot_json) VALUES (?,?)`, id, tc.merge); err != nil {
				t.Fatal(err)
			}
		}
	}
	runs, err := s.tickRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	selected := map[string]bool{}
	for _, run := range runs {
		if selected[run.ID] {
			t.Fatalf("run %s selected twice", run.ID)
		}
		selected[run.ID] = true
	}
	for id, want := range want {
		if selected[id] != want {
			t.Errorf("run %s selected=%v, want %v", id, selected[id], want)
		}
	}
	if len(runs) != countSelected(want) {
		t.Fatalf("selected %d runs, want %d", len(runs), countSelected(want))
	}
}

func countSelected(want map[string]bool) int {
	count := 0
	for _, selected := range want {
		if selected {
			count++
		}
	}
	return count
}

// Every cleanup step maintenance.Snapshot reports as remaining must keep its run
// visible to a tick. Adding a step without extending tickRunCondition fails here
// instead of silently abandoning pending cleanup.
func TestTickRunsFollowsEveryMaintenancePendingStep(t *testing.T) {
	ctx := context.Background()
	db, _ := historyStore(t)
	s := historyScheduler(t, db)
	done := maintenance.Snapshot{
		ObservedAt: "2026-01-01T00:00:00Z", ProcessExited: true,
		ReadyRemoved: true, RunningRemoved: true, AttentionRemoved: true, IssueClosed: true,
		ReviewRestored: true, WorktreeStarted: true, WorktreeRemoved: true, BranchDeleted: true,
	}
	typ := reflect.TypeOf(done)
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Type.Kind() != reflect.Bool {
			continue
		}
		id := "maintenance-" + field.Name
		v := done
		reflect.ValueOf(&v).Elem().Field(i).SetBool(false)
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO runs (id,repository,issue_number,state,current_phase,review_round) VALUES (?,?,?,?,'implement',0)`, id, historyRepo, 20000+i, string(workflow.Completed)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO merge_cleanup (run_id,snapshot_json) VALUES (?,?)`, id, string(data)); err != nil {
			t.Fatal(err)
		}
		runs, err := s.tickRuns(ctx)
		if err != nil {
			t.Fatal(err)
		}
		selected := slices.ContainsFunc(runs, func(run workflow.Run) bool { return run.ID == id })
		if selected != v.Pending() {
			t.Errorf("single remaining step %s: selected=%v, Snapshot.Pending()=%v", field.Name, selected, v.Pending())
		}
	}
}
