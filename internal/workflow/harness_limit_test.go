package workflow_test

import (
	"context"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestHarnessRestrictionResetBoundary(t *testing.T) {
	ctx := context.Background()
	reset := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for _, reason := range []string{"temporary_limit", "credits_exhausted"} {
		t.Run(reason, func(t *testing.T) {
			_, _, db := newWorkflow(t)
			_, err := db.ExecContext(ctx, `INSERT INTO harness_limits(harness_type,reason,limited_until,reset_time_source,restriction_id) VALUES('claude',?,?,'reported','generation')`, reason, reset.Format(time.RFC3339Nano))
			if err != nil {
				t.Fatal(err)
			}
			limit, err := workflow.LoadHarnessLimit(ctx, db, "claude")
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name             string
				now              time.Time
				available, check bool
				displayReset     time.Time
			}{
				{"before", reset.Add(-time.Nanosecond), false, false, reset},
				{"at", reset, reason == "temporary_limit", reason == "credits_exhausted", time.Time{}},
				{"after", reset.Add(time.Nanosecond), reason == "temporary_limit", reason == "credits_exhausted", time.Time{}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if got := limit.Available(tc.now); got != tc.available {
						t.Fatalf("available = %v", got)
					}
					if got := limit.CheckEligible(tc.now, true); got != tc.check {
						t.Fatalf("check eligible = %v", got)
					}
					if limit.CheckEligible(tc.now, false) {
						t.Fatal("check allowed without detection capability")
					}
					gate := limit.Gate(tc.now)
					if tc.available {
						if gate != nil {
							t.Fatalf("available harness gated: %+v", gate)
						}
						return
					}
					display := gate.Display(tc.now)
					if display.ResetAt != tc.displayReset {
						t.Fatalf("display reset = %v, want %v", display.ResetAt, tc.displayReset)
					}
					if display.ResetAt.IsZero() && display.ResetTimeSource != "" {
						t.Fatal("expired provenance displayed")
					}
				})
			}
			saved, err := workflow.LoadHarnessLimit(ctx, db, "claude")
			if err != nil || saved.ResetAt != reset || saved.ResetTimeSource != "reported" {
				t.Fatalf("authority changed: %+v, %v", saved, err)
			}
			missing, err := workflow.LoadHarnessLimit(ctx, db, "codex")
			if err != nil || !missing.Available(reset) || missing.Gate(reset) != nil || missing.CheckEligible(reset, true) {
				t.Fatalf("unrestricted harness = %+v, %v", missing, err)
			}
		})
	}
}

func TestHarnessRestrictionSelectedProbe(t *testing.T) {
	ctx := context.Background()
	w, _, db := newWorkflow(t)
	run := seed(t, w, workflow.NeedsAttention, workflow.Implement)
	reset := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	if _, err := db.ExecContext(ctx, `INSERT INTO harness_limits(harness_type,reason,limited_until,reset_time_source,restriction_id) VALUES('claude','credits_exhausted',?,'reported','generation')`, reset.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	load := func() *workflow.HarnessLimit {
		t.Helper()
		v, err := workflow.LoadHarnessLimit(ctx, db, "claude")
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	// Retry remains requestable during a reset; that is not permission to launch.
	if !load().RecoveryEligible(true) || load().RecoveryEligible(false) {
		t.Fatal("recovery request capability was not respected")
	}
	probe := workflow.CreditProbe{ID: "probe", Harness: "claude", RestrictionID: "generation", RunID: run.ID}
	if _, err := w.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.Retry, NextState: workflow.Active, NextPhase: workflow.Implement, Metadata: workflow.MetadataPatch{CreditProbe: &probe}}); err != nil {
		t.Fatal(err)
	}
	v := load()
	for _, tc := range []struct {
		name             string
		runID            string
		now              time.Time
		detection, ready bool
	}{
		{"before reset", run.ID, reset.Add(-time.Nanosecond), true, false},
		{"at reset", run.ID, reset, true, true},
		{"after reset", run.ID, reset.Add(time.Nanosecond), true, true},
		{"other run", "other", reset, true, false},
		{"no detection", run.ID, reset, false, false},
		{"no run", "", reset, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if v.ProbeReady(tc.runID, tc.now, tc.detection) != tc.ready {
				t.Fatalf("probe permission = %v", !tc.ready)
			}
			if v.Available(tc.now) || v.Gate(tc.now) == nil || v.RecoveryEligible(true) || v.CheckEligible(tc.now, true) {
				t.Fatal("selected probe made the harness available or permitted another probe")
			}
		})
	}
	if !v.OwnsProbe(probe) {
		t.Fatal("selected generation not owned")
	}
	stale := probe
	stale.RestrictionID = "older"
	if v.OwnsProbe(stale) {
		t.Fatal("older restriction owns current probe")
	}
	// A standalone check is the same single-owner gate and cannot bypass a run.
	if err := w.ReserveHarnessCheck(ctx, workflow.HarnessCheck{ID: "check", Harness: "claude", RestrictionID: "generation"}, reset); err == nil {
		t.Fatal("check bypassed selected run")
	}
	// Launch binding is tested with the transaction and persisted attempt it uses.
	if _, err := db.ExecContext(ctx, `INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,status,agent) VALUES('execution',?,'implement','implementer',0,1,'running','claude')`, run.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := workflow.BindCreditProbe(ctx, tx, run.ID, "claude", "execution", reset); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if load().ProbeReady(run.ID, reset, true) {
		t.Fatal("bound probe may launch twice")
	}
	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = workflow.BindCreditProbe(ctx, tx, run.ID, "claude", "execution", reset)
	tx.Rollback()
	if err == nil {
		t.Fatal("second launch binding accepted")
	}
}
