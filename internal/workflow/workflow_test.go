package workflow_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/store"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestClaimPersistsRunAndOneEvent(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var logs bytes.Buffer
	bus := events.New(db, slog.New(slog.NewJSONHandler(&logs, nil)))
	w := workflow.New(db, bus)
	live, cancel := bus.Subscribe(1)
	defer cancel()
	run, err := w.Transition(ctx, "run-5", workflow.Request{
		Trigger: workflow.IssueClaimed, Repository: "rcpassos/mergeyard", IssueNumber: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.State != workflow.Claiming || run.Phase != "" {
		t.Fatalf("claim = %+v", run)
	}
	saved, err := w.Get(ctx, run.ID)
	if err != nil || saved != run {
		t.Fatalf("saved = %+v, error = %v; want %+v", saved, err, run)
	}
	history, err := bus.History(ctx, 0, 100)
	if err != nil || len(history) != 1 || history[0].Type != "run.claimed" || history[0].RunID != run.ID {
		t.Fatalf("history = %+v, error = %v", history, err)
	}
	select {
	case event := <-live:
		if event.ID != history[0].ID || string(event.Payload) != string(history[0].Payload) {
			t.Fatalf("live event differs from persisted event: %+v", event)
		}
	default:
		t.Fatal("committed event was not delivered")
	}
	if !strings.Contains(logs.String(), `"event_type":"run.claimed"`) {
		t.Fatalf("missing structured event log: %s", &logs)
	}
}

func newWorkflow(t *testing.T) (*workflow.Workflow, *events.Bus, *sql.DB) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	bus := events.New(db, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	return workflow.New(db, bus), bus, db
}

func seed(t *testing.T, w *workflow.Workflow, state workflow.State, phase workflow.Phase) workflow.Run {
	t.Helper()
	requests := []workflow.Request{{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 5}}
	if state != workflow.Claiming {
		requests = append(requests, workflow.Request{Trigger: workflow.ClaimSucceeded})
	}
	if state != workflow.Claiming && state != workflow.Preparing {
		requests = append(requests, workflow.Request{Trigger: workflow.WorktreeReady})
		if phase == workflow.Review || phase == workflow.Fix || state == workflow.WaitingForCI || state == workflow.ReadyToMerge || state == workflow.Completed {
			requests = append(requests, workflow.Request{Trigger: workflow.ImplementSucceeded})
		}
		if phase == workflow.Fix {
			requests = append(requests, workflow.Request{Trigger: workflow.ReviewChangesRequired})
		}
		switch state {
		case workflow.WaitingForCI, workflow.ReadyToMerge, workflow.Completed:
			requests = append(requests, workflow.Request{Trigger: workflow.ReviewApproved})
			if state != workflow.WaitingForCI {
				requests = append(requests, workflow.Request{Trigger: workflow.CIPassed})
			}
			if state == workflow.Completed {
				requests = append(requests, workflow.Request{Trigger: workflow.PRMerged})
			}
		case workflow.WaitingForHarness:
			requests = append(requests, workflow.Request{Trigger: workflow.HarnessLimited})
		case workflow.Manual:
			requests = append(requests, workflow.Request{Trigger: workflow.TakeOver})
		case workflow.NeedsAttention:
			requests = append(requests, workflow.Request{Trigger: workflow.PhaseBlocked, Failure: blocked()})
		case workflow.Failed:
			requests = append(requests, workflow.Request{Trigger: workflow.InternalFailure, Failure: blocked()})
		case workflow.Stopped:
			requests = append(requests, workflow.Request{Trigger: workflow.Stop})
		}
	}
	var run workflow.Run
	for _, request := range requests {
		var err error
		run, err = w.Transition(context.Background(), "run", request)
		if err != nil {
			t.Fatalf("seed %s/%s with %s: %v", state, phase, request.Trigger, err)
		}
	}
	return run
}

func blocked() *fault.Error {
	return &fault.Error{Code: "phase.blocked", Message: "The issue needs clarification"}
}

func TestFixedTransitionRows(t *testing.T) {
	rows := []struct {
		from    workflow.State
		phase   workflow.Phase
		trigger workflow.Trigger
		to      workflow.State
		next    workflow.Phase
	}{
		{workflow.Claiming, "", workflow.ClaimSucceeded, workflow.Preparing, ""},
		{workflow.Preparing, "", workflow.WorktreeReady, workflow.Active, workflow.Implement},
		{workflow.Active, workflow.Implement, workflow.ImplementSucceeded, workflow.Active, workflow.Review},
		{workflow.Active, workflow.Review, workflow.ReviewApproved, workflow.WaitingForCI, workflow.Review},
		{workflow.Active, workflow.Review, workflow.ReviewChangesRequired, workflow.Active, workflow.Fix},
		{workflow.Active, workflow.Review, workflow.ReviewRoundsExhausted, workflow.NeedsAttention, workflow.Review},
		{workflow.Active, workflow.Fix, workflow.FixSucceeded, workflow.Active, workflow.Review},
		{workflow.WaitingForCI, workflow.Review, workflow.CIPassed, workflow.ReadyToMerge, workflow.Review},
		{workflow.WaitingForCI, workflow.Review, workflow.CIFailed, workflow.Active, workflow.Fix},
		{workflow.WaitingForCI, workflow.Review, workflow.CIRoundsExhausted, workflow.NeedsAttention, workflow.Review},
		{workflow.ReadyToMerge, workflow.Review, workflow.PRMerged, workflow.Completed, workflow.Review},
		{workflow.ReadyToMerge, workflow.Review, workflow.PRClosedUnmerged, workflow.NeedsAttention, workflow.Review},
		{workflow.WaitingForHarness, workflow.Implement, workflow.HarnessWaitsExhausted, workflow.NeedsAttention, workflow.Implement},
	}
	for _, row := range rows {
		t.Run(string(row.from)+"/"+string(row.phase)+"/"+string(row.trigger), func(t *testing.T) {
			w, bus, _ := newWorkflow(t)
			seed(t, w, row.from, row.phase)
			before, _ := bus.History(context.Background(), 0, 100)
			request := workflow.Request{Trigger: row.trigger}
			if row.to == workflow.NeedsAttention {
				request.Failure = blocked()
			}
			run, err := w.Transition(context.Background(), "run", request)
			if err != nil || run.State != row.to || run.Phase != row.next {
				t.Fatalf("transition = %+v, %v; want %s/%s", run, err, row.to, row.next)
			}
			saved, err := w.Get(context.Background(), "run")
			if err != nil || saved != run {
				t.Fatalf("transition was not persisted: %+v, %v", saved, err)
			}
			after, err := bus.History(context.Background(), before[len(before)-1].ID, 100)
			if err != nil || len(after) != 1 {
				t.Fatalf("transition events = %+v, %v; want exactly one", after, err)
			}
			if row.to == workflow.NeedsAttention && (run.LastErrorCode != "phase.blocked" || run.LastErrorMessage != "The issue needs clarification") {
				t.Fatalf("missing actionable error: %+v", run)
			}
			if (row.to == workflow.Completed) != (run.CompletedAt != "") {
				t.Fatalf("incorrect terminal timestamp: %+v", run)
			}
		})
	}
}

func assertInvalid(t *testing.T, w *workflow.Workflow, bus *events.Bus, request workflow.Request) {
	t.Helper()
	ctx := context.Background()
	before, err := w.Get(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	history, _ := bus.History(ctx, 0, 100)
	_, err = w.Transition(ctx, "run", request)
	var failure *fault.Error
	if !errors.As(err, &failure) || failure.Code != "internal.invalid_transition" {
		t.Fatalf("expected coded invalid transition, got %v", err)
	}
	after, err := w.Get(ctx, "run")
	if err != nil || after != before {
		t.Fatalf("rejected transition changed the run: %+v, %v", after, err)
	}
	added, err := bus.History(ctx, history[len(history)-1].ID, 100)
	if err != nil || len(added) != 0 {
		t.Fatalf("rejected transition emitted events: %+v, %v", added, err)
	}
}

func TestWildcardTransitionRows(t *testing.T) {
	ctx := context.Background()
	for _, phase := range []workflow.Phase{workflow.Implement, workflow.Review, workflow.Fix} {
		for _, trigger := range []workflow.Trigger{workflow.HarnessLimited, workflow.PhaseBlocked, workflow.ResultInvalid, workflow.PhaseRetriesExhausted} {
			t.Run(string(phase)+"/"+string(trigger), func(t *testing.T) {
				w, bus, _ := newWorkflow(t)
				seed(t, w, workflow.Active, phase)
				before, _ := bus.History(ctx, 0, 100)
				request := workflow.Request{Trigger: trigger}
				want := workflow.WaitingForHarness
				if trigger != workflow.HarnessLimited {
					request.Failure, want = blocked(), workflow.NeedsAttention
				}
				run, err := w.Transition(ctx, "run", request)
				if err != nil || run.State != want || run.Phase != phase {
					t.Fatalf("transition = %+v, %v", run, err)
				}
				assertOneEvent(t, bus, before[len(before)-1].ID)
				if trigger == workflow.HarnessLimited {
					before, _ = bus.History(ctx, 0, 100)
					run, err = w.Transition(ctx, "run", workflow.Request{Trigger: workflow.HarnessAvailable})
					if err != nil || run.State != workflow.Active || run.Phase != phase {
						t.Fatalf("resume did not retain interrupted phase: %+v, %v", run, err)
					}
					assertOneEvent(t, bus, before[len(before)-1].ID)
				}
			})
		}
	}
	states := []workflow.State{workflow.Claiming, workflow.Preparing, workflow.Active, workflow.WaitingForCI, workflow.WaitingForHarness,
		workflow.ReadyToMerge, workflow.Manual, workflow.NeedsAttention, workflow.Failed, workflow.Stopped, workflow.Completed}
	for _, state := range states {
		for _, trigger := range []workflow.Trigger{workflow.TakeOver, workflow.Stop, workflow.InternalFailure} {
			t.Run(string(state)+"/"+string(trigger), func(t *testing.T) {
				w, bus, _ := newWorkflow(t)
				seed(t, w, state, workflow.Implement)
				request := workflow.Request{Trigger: trigger}
				if (trigger == workflow.TakeOver && state.Terminal()) || (trigger == workflow.Stop && state == workflow.Completed) {
					assertInvalid(t, w, bus, request)
					return
				}
				want := workflow.Manual
				if trigger == workflow.Stop {
					want = workflow.Stopped
				} else if trigger == workflow.InternalFailure {
					want, request.Failure = workflow.Failed, blocked()
				}
				before, _ := bus.History(ctx, 0, 100)
				run, err := w.Transition(ctx, "run", request)
				if err != nil || run.State != want {
					t.Fatalf("transition = %+v, %v; want %s", run, err, want)
				}
				if trigger == workflow.InternalFailure && (run.LastErrorCode != "phase.blocked" || run.LastErrorMessage != "The issue needs clarification") {
					t.Fatalf("failed run lost its error: %+v", run)
				}
				if want.Terminal() != (run.CompletedAt != "") {
					t.Fatalf("terminal timestamp = %q for %s", run.CompletedAt, want)
				}
				assertOneEvent(t, bus, before[len(before)-1].ID)
			})
		}
	}
}

func assertOneEvent(t *testing.T, bus *events.Bus, after int64) {
	t.Helper()
	history, err := bus.History(context.Background(), after, 100)
	if err != nil || len(history) != 1 {
		t.Fatalf("transition events = %+v, %v; want exactly one", history, err)
	}
}

func TestHandBackAndReconciledRetryRows(t *testing.T) {
	ctx := context.Background()
	for _, phase := range []workflow.Phase{workflow.Implement, workflow.Review, workflow.Fix} {
		t.Run("handback/"+string(phase), func(t *testing.T) {
			w, bus, _ := newWorkflow(t)
			seed(t, w, workflow.Manual, workflow.Implement)
			before, _ := bus.History(ctx, 0, 100)
			run, err := w.Transition(ctx, "run", workflow.Request{Trigger: workflow.HandBack, NextPhase: phase})
			if err != nil || run.State != workflow.Active || run.Phase != phase {
				t.Fatalf("handback = %+v, %v", run, err)
			}
			assertOneEvent(t, bus, before[len(before)-1].ID)
		})
	}
	for _, from := range []workflow.State{workflow.NeedsAttention, workflow.Failed} {
		for _, to := range []workflow.State{workflow.Claiming, workflow.Preparing, workflow.Active, workflow.WaitingForCI, workflow.WaitingForHarness, workflow.ReadyToMerge, workflow.Completed} {
			t.Run(string(from)+"/retry/"+string(to), func(t *testing.T) {
				w, bus, _ := newWorkflow(t)
				seed(t, w, from, workflow.Implement)
				before, _ := bus.History(ctx, 0, 100)
				request := workflow.Request{Trigger: workflow.Retry, NextState: to}
				if to == workflow.Active || to == workflow.WaitingForHarness {
					request.NextPhase = workflow.Review
				}
				run, err := w.Transition(ctx, "run", request)
				if err != nil || run.State != to || run.ID != "run" || run.LastErrorCode != "" || run.LastErrorMessage != "" {
					t.Fatalf("retry = %+v, %v", run, err)
				}
				if (to == workflow.Completed) != (run.CompletedAt != "") {
					t.Fatalf("retry has incorrect terminal timestamp: %+v", run)
				}
				assertOneEvent(t, bus, before[len(before)-1].ID)
			})
		}
	}
}

func TestInvalidStateTriggerPairs(t *testing.T) {
	// Allowed pairs come from PRD §21, with FAILED retry from §22. Phase-specific
	// pairs are checked separately below; every other pair must be rejected.
	allowed := map[workflow.State][]workflow.Trigger{
		workflow.Claiming:  {workflow.ClaimSucceeded, workflow.TakeOver, workflow.Stop, workflow.InternalFailure},
		workflow.Preparing: {workflow.WorktreeReady, workflow.TakeOver, workflow.Stop, workflow.InternalFailure},
		workflow.Active: {workflow.ImplementSucceeded, workflow.ReviewApproved, workflow.ReviewChangesRequired, workflow.ReviewRoundsExhausted,
			workflow.FixSucceeded, workflow.HarnessLimited, workflow.PhaseBlocked, workflow.ResultInvalid, workflow.PhaseRetriesExhausted, workflow.TakeOver, workflow.Stop, workflow.InternalFailure},
		workflow.WaitingForCI:      {workflow.CIPassed, workflow.CIFailed, workflow.CIRoundsExhausted, workflow.TakeOver, workflow.Stop, workflow.InternalFailure},
		workflow.WaitingForHarness: {workflow.HarnessAvailable, workflow.HarnessWaitsExhausted, workflow.TakeOver, workflow.Stop, workflow.InternalFailure},
		workflow.ReadyToMerge:      {workflow.PRMerged, workflow.PRClosedUnmerged, workflow.TakeOver, workflow.Stop, workflow.InternalFailure},
		workflow.Manual:            {workflow.HandBack, workflow.TakeOver, workflow.Stop, workflow.InternalFailure},
		workflow.NeedsAttention:    {workflow.Retry, workflow.TakeOver, workflow.Stop, workflow.InternalFailure},
		workflow.Failed:            {workflow.Retry, workflow.Stop, workflow.InternalFailure},
		workflow.Stopped:           {workflow.Stop, workflow.InternalFailure},
		workflow.Completed:         {workflow.InternalFailure},
	}
	triggers := []workflow.Trigger{workflow.IssueClaimed, workflow.ClaimSucceeded, workflow.WorktreeReady, workflow.ImplementSucceeded,
		workflow.ReviewApproved, workflow.ReviewChangesRequired, workflow.ReviewRoundsExhausted, workflow.FixSucceeded,
		workflow.CIPassed, workflow.CIFailed, workflow.CIRoundsExhausted, workflow.PRMerged, workflow.PRClosedUnmerged,
		workflow.HarnessLimited, workflow.HarnessAvailable, workflow.HarnessWaitsExhausted, workflow.TakeOver, workflow.HandBack,
		workflow.PhaseBlocked, workflow.ResultInvalid, workflow.PhaseRetriesExhausted, workflow.Retry, workflow.Stop, workflow.InternalFailure, "unknown"}
	for state, permitted := range allowed {
		t.Run(string(state), func(t *testing.T) {
			w, bus, _ := newWorkflow(t)
			seed(t, w, state, workflow.Implement)
			for _, trigger := range triggers {
				valid := false
				for _, candidate := range permitted {
					valid = valid || candidate == trigger
				}
				if !valid {
					t.Run(string(trigger), func(t *testing.T) {
						assertInvalid(t, w, bus, workflow.Request{Trigger: trigger, Failure: blocked()})
					})
				}
			}
		})
	}
	for _, phase := range []workflow.Phase{workflow.Implement, workflow.Review, workflow.Fix} {
		t.Run(string(phase), func(t *testing.T) {
			w, bus, _ := newWorkflow(t)
			seed(t, w, workflow.Active, phase)
			for _, trigger := range []workflow.Trigger{workflow.ImplementSucceeded, workflow.ReviewApproved, workflow.ReviewChangesRequired, workflow.ReviewRoundsExhausted, workflow.FixSucceeded} {
				valid := (phase == workflow.Implement && trigger == workflow.ImplementSucceeded) ||
					(phase == workflow.Fix && trigger == workflow.FixSucceeded) ||
					(phase == workflow.Review && trigger != workflow.ImplementSucceeded && trigger != workflow.FixSucceeded)
				if !valid {
					assertInvalid(t, w, bus, workflow.Request{Trigger: trigger, Failure: blocked()})
				}
			}
		})
	}
}

func TestInvalidDestinationsAndMissingErrors(t *testing.T) {
	for _, row := range []struct {
		state   workflow.State
		request workflow.Request
	}{
		{workflow.Active, workflow.Request{Trigger: workflow.PhaseBlocked}},
		{workflow.Active, workflow.Request{Trigger: workflow.InternalFailure, Failure: &fault.Error{Code: "internal.failure"}}},
		{workflow.Active, workflow.Request{Trigger: workflow.PhaseBlocked, Failure: &fault.Error{Message: "No error code"}}},
		{workflow.Active, workflow.Request{Trigger: workflow.ImplementSucceeded, NextState: workflow.Completed}},
		{workflow.Active, workflow.Request{Trigger: workflow.ImplementSucceeded, NextPhase: workflow.Fix}},
		{workflow.Manual, workflow.Request{Trigger: workflow.HandBack}},
		{workflow.Manual, workflow.Request{Trigger: workflow.HandBack, NextPhase: "unknown"}},
		{workflow.Manual, workflow.Request{Trigger: workflow.HandBack, NextPhase: workflow.Review, NextState: workflow.Completed}},
		{workflow.NeedsAttention, workflow.Request{Trigger: workflow.Retry}},
		{workflow.NeedsAttention, workflow.Request{Trigger: workflow.Retry, NextState: workflow.Active}},
		{workflow.NeedsAttention, workflow.Request{Trigger: workflow.Retry, NextState: workflow.WaitingForHarness}},
		{workflow.NeedsAttention, workflow.Request{Trigger: workflow.Retry, NextState: workflow.Active, NextPhase: "unknown"}},
		{workflow.NeedsAttention, workflow.Request{Trigger: workflow.Retry, NextState: workflow.Manual}},
		{workflow.NeedsAttention, workflow.Request{Trigger: workflow.Retry, NextState: workflow.NeedsAttention}},
		{workflow.NeedsAttention, workflow.Request{Trigger: workflow.Retry, NextState: workflow.Failed}},
		{workflow.NeedsAttention, workflow.Request{Trigger: workflow.Retry, NextState: workflow.Stopped}},
		{workflow.NeedsAttention, workflow.Request{Trigger: workflow.Retry, NextState: "unknown"}},
	} {
		t.Run(string(row.state)+"/"+string(row.request.Trigger)+"/"+string(row.request.NextState)+"/"+string(row.request.NextPhase), func(t *testing.T) {
			w, bus, _ := newWorkflow(t)
			seed(t, w, row.state, workflow.Implement)
			assertInvalid(t, w, bus, row.request)
		})
	}
}

func TestEventFailureRollsBackTransition(t *testing.T) {
	ctx := context.Background()
	w, bus, db := newWorkflow(t)
	before := seed(t, w, workflow.Preparing, "")
	history, _ := bus.History(ctx, 0, 100)
	live, cancel := bus.Subscribe(1)
	defer cancel()
	// Inject an SQLite event-write failure at the external storage boundary.
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER reject_event BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT, 'event storage unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Transition(ctx, "run", workflow.Request{Trigger: workflow.WorktreeReady}); err == nil {
		t.Fatal("transition succeeded when the event could not be stored")
	}
	after, err := w.Get(ctx, "run")
	if err != nil || after != before {
		t.Fatalf("rolled-back run = %+v, %v; want %+v", after, err, before)
	}
	added, err := bus.History(ctx, history[len(history)-1].ID, 100)
	if err != nil || len(added) != 0 {
		t.Fatalf("rollback events = %+v, %v", added, err)
	}
	select {
	case event := <-live:
		t.Fatalf("rolled-back event broadcast: %+v", event)
	default:
	}
}

func TestConcurrentTransitionsObserveCommittedState(t *testing.T) {
	w, bus, _ := newWorkflow(t)
	seed(t, w, workflow.Claiming, "")
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := w.Transition(context.Background(), "run", workflow.Request{Trigger: workflow.ClaimSucceeded})
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else {
			var failure *fault.Error
			if !errors.As(err, &failure) || failure.Code != "internal.invalid_transition" {
				t.Fatalf("unexpected competing transition error: %v", err)
			}
		}
	}
	history, err := bus.History(context.Background(), 0, 100)
	if err != nil || successes != 1 || len(history) != 2 {
		t.Fatalf("successes = %d, history = %+v, error = %v", successes, history, err)
	}
}

func TestDuplicateClaimAndConflictingRetryAreAtomic(t *testing.T) {
	ctx := context.Background()
	w, bus, _ := newWorkflow(t)
	failed := seed(t, w, workflow.Failed, workflow.Implement)
	request := workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 5}
	if _, err := w.Transition(ctx, "new-run", request); err != nil {
		t.Fatal(err)
	}
	history, _ := bus.History(ctx, 0, 100)
	if _, err := w.Transition(ctx, "duplicate", request); err == nil {
		t.Fatal("allowed a second non-terminal run for the issue")
	}
	if _, err := w.Get(ctx, "duplicate"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("failed claim left a run behind: %v", err)
	}
	if _, err := w.Transition(ctx, "run", workflow.Request{Trigger: workflow.Retry, NextState: workflow.Active, NextPhase: workflow.Implement}); err == nil {
		t.Fatal("reactivated a failed run alongside an active run for the issue")
	}
	got, err := w.Get(ctx, "run")
	if err != nil || got != failed {
		t.Fatalf("conflicting retry changed failed history: %+v, %v", got, err)
	}
	added, err := bus.History(ctx, history[len(history)-1].ID, 100)
	if err != nil || len(added) != 0 {
		t.Fatalf("conflicts produced events: %+v, %v", added, err)
	}
}
