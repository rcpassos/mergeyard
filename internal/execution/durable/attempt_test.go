package durable_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/execution/durable"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type ambiguousRunner struct {
	runner.Runner
	starts int
	before func()
}

func (r *ambiguousRunner) StartSession(ctx context.Context, req runner.SessionRequest) (runner.SessionRef, error) {
	r.starts++
	if r.before != nil {
		r.before()
	}
	ref, err := r.Runner.StartSession(ctx, req)
	if err != nil {
		return ref, err
	}
	return ref, errors.New("launcher response lost")
}

func launch(t *testing.T, e durable.Executor, run workflow.Run, root string, role config.Role, number int, script string) (harness.PhaseContext, durable.Attempt, error) {
	t.Helper()
	ctx := context.Background()
	phase, previous, err := e.PrepareImplement(ctx, root, run, role, number)
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(phase.PhaseDir, "input.json")
	if err := e.Runner.WriteFile(ctx, input, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := e.LaunchImplement(ctx, run, role, harness.Permissions{Summary: "test permissions"}, phase, previous, runner.SessionRequest{RunID: run.ID, Phase: "implement", Attempt: number, PhaseDir: phase.PhaseDir, Command: runner.ExecRequest{Executable: "/bin/sh", Args: []string{"-c", script}, Dir: root, Env: map[string]string{"PATH": "/usr/bin:/bin"}}}, input, "")
	return phase, a, err
}

func TestAmbiguousLaunchIsObservedAfterRestart(t *testing.T) {
	e, w, run, root := executor(t)
	r := &ambiguousRunner{Runner: runner.NewLocal(runner.Options{SocketName: "durable-" + filepath.Base(root)})}
	e.Runner = r
	r.before = func() {
		saved, err := e.LoadImplement(context.Background(), run.ID)
		if err != nil || saved.Status != "running" || saved.Ref.Name == "" {
			t.Fatalf("launch missing durable intent: %+v %v", saved, err)
		}
		current, err := w.Get(context.Background(), run.ID)
		if err != nil || current.Implementer == nil || current.Implementer.SessionID == "" {
			t.Fatalf("launch missing role identity: %+v %v", current, err)
		}
		history, err := e.Events.History(context.Background(), 0, 100)
		if err != nil || history[len(history)-1].Type != "phase.attempt_started" {
			t.Fatalf("launch event: %+v %v", history, err)
		}
	}
	phase, a, err := launch(t, e, run, root, config.Role{Agent: "claude", Model: "saved-model", Effort: "high", Skills: []string{"implement"}}, 1, "printf 'captured output\\n'")
	if err != nil {
		t.Fatal(err)
	}
	defer r.StopSession(context.Background(), a.Ref)
	// Reopen storage and create a new runner: no in-memory execution state survives.
	if err := e.DB.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := openAgain(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e.DB = db
	e.Runner = runner.NewLocal(runner.Options{SocketName: "durable-" + filepath.Base(root)})
	saved, err := e.LoadImplement(context.Background(), run.ID)
	if err != nil || saved.ID != a.ID || saved.SessionID != phase.SessionID {
		t.Fatalf("restart: %+v %v", saved, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		o, err := e.Observe(context.Background(), run.ID, workflow.Implement, &saved)
		if err != nil || o.Failure != nil {
			t.Fatalf("observation: %+v %v", o, err)
		}
		if o.Status.State == runner.SessionExited {
			if string(o.Artifacts.Stdout) != "captured output\n" {
				t.Fatalf("stdout=%q", o.Artifacts.Stdout)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("attempt did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r.starts != 1 {
		t.Fatalf("ambiguous launch replayed: %d", r.starts)
	}
}

func TestAttemptRoundKeepsPhasesOnTheSharedRound(t *testing.T) {
	run := workflow.Run{RunMetadata: workflow.RunMetadata{ReviewRound: 2}}
	for _, tc := range []struct {
		phase workflow.Phase
		want  int
	}{
		{workflow.Implement, 0},
		{workflow.Review, 2},
		{workflow.Fix, 2},
	} {
		if got := durable.AttemptRound(run, tc.phase); got != tc.want {
			t.Errorf("%s round = %d; want %d", tc.phase, got, tc.want)
		}
	}
}

func TestInvalidDiscoveryRecognizesOnlyInvalidatedSessions(t *testing.T) {
	for _, code := range []string{"phase.result_invalid", "harness.session_identity_invalid", "harness.session_resume_failed"} {
		if !durable.InvalidDiscovery(&fault.Error{Code: code}) {
			t.Errorf("%s not recognized as invalid discovery", code)
		}
	}
	wrapped := fmt.Errorf("observe: %w", &fault.Error{Code: "harness.session_identity_invalid"})
	if !durable.InvalidDiscovery(wrapped) {
		t.Errorf("wrapped failure not recognized as invalid discovery")
	}
	for _, err := range []error{errors.New("plain failure"), &fault.Error{Code: "phase.session_missing"}} {
		if durable.InvalidDiscovery(err) {
			t.Errorf("%v recognized as invalid discovery", err)
		}
	}
}

func TestMissingConversationReplacementIsReservedBeforeLaunch(t *testing.T) {
	e, w, run, root := executor(t)
	r := &rejectRunner{Runner: e.Runner}
	e.Runner = r
	_, first, err := launch(t, e, run, root, config.Role{Agent: "claude"}, 1, "exit 0")
	if err == nil || first.Status != "failed" {
		t.Fatalf("definite startup: %+v %v", first, err)
	}
	r.before = func() {
		saved, err := w.Get(context.Background(), run.ID)
		if err != nil || len(saved.SessionRecoveries) != 1 || saved.Implementer.SessionID == first.SessionID {
			t.Fatalf("replacement launch: %+v %v", saved, err)
		}
	}
	_, second, err := launch(t, e, run, root, config.Role{Agent: "claude"}, 2, "exit 0")
	if err == nil || second.Resumed || second.SessionID == first.SessionID {
		t.Fatalf("replacement: %+v %v", second, err)
	}
	// A duplicate reservation must roll back the new identity and attempt together.
	r.before = func() { t.Fatal("process started despite recovery reservation failure") }
	_, third, err := launch(t, e, run, root, config.Role{Agent: "claude"}, 3, "exit 0")
	if err == nil || third.ID != "" {
		t.Fatalf("duplicate reservation: %+v %v", third, err)
	}
	latest, err := e.LoadImplement(context.Background(), run.ID)
	if err != nil || latest.ID != second.ID {
		t.Fatalf("rollback: %+v %v", latest, err)
	}
}

type rejectRunner struct {
	runner.Runner
	before func()
}

func (r *rejectRunner) StartSession(context.Context, runner.SessionRequest) (runner.SessionRef, error) {
	if r.before != nil {
		r.before()
	}
	return runner.SessionRef{}, &fault.Error{Code: "harness.session_resume_failed", Message: "saved conversation missing"}
}

func TestPrepareRequiresProcessJournal(t *testing.T) {
	e, _, run, root := executor(t)
	dir := filepath.Join(root, "runs", run.ID, "phases", "implement-0-1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// The marker cannot be written, so no executable intent should be recorded.
	// Use a file at the attempt directory path to exercise preparation failure.
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, nil, 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err := e.PrepareImplement(context.Background(), root, run, config.Role{Agent: "claude"}, 1)
	if err == nil {
		t.Fatal("preparation succeeded without a journal directory")
	}
}

func TestDiscoveredConversationIsDurableAndIndependent(t *testing.T) {
	e, _, run, root := executor(t)
	ctx := context.Background()
	const identity = "01a10c5b-38a9-7330-b833-04e365cb5f37"
	const reviewer = "01a10c5b-38a9-7330-b833-04e365cb5f38"
	if _, err := e.DB.ExecContext(ctx, "UPDATE runs SET reviewer_session_id=? WHERE id=?", reviewer, run.ID); err != nil {
		t.Fatal(err)
	}
	e.Runner = runner.NewLocal(runner.Options{SocketName: "discover-" + filepath.Base(root)})
	_, a, err := launch(t, e, run, root, config.Role{Agent: "codex"}, 1, `printf '%s\n' '{"type":"thread.started","thread_id":"`+identity+`"}'`)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Runner.StopSession(ctx, a.Ref)
	deadline := time.Now().Add(5 * time.Second)
	for {
		o, err := e.Observe(ctx, run.ID, workflow.Implement, &a)
		if err != nil || o.Failure != nil {
			t.Fatalf("discovery: %+v %v", o, err)
		}
		if o.Status.State == runner.SessionExited {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("discovery did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	saved, err := e.LoadImplement(ctx, run.ID)
	if err != nil || saved.SessionID != identity {
		t.Fatalf("saved discovery: %+v %v", saved, err)
	}
	phase, _, err := e.PrepareImplement(ctx, root, run, config.Role{Agent: "codex"}, 2)
	if err != nil || !phase.Resume || phase.SessionID != identity {
		t.Fatalf("resume: %+v %v", phase, err)
	}
	// A new capture cannot claim the reviewer's independent conversation.
	if err := e.Runner.WriteFile(ctx, filepath.Join(a.Ref.PhaseDir, "events.jsonl"), []byte(`{"type":"thread.started","thread_id":"`+reviewer+`"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	err = e.Discover(ctx, run.ID, workflow.Implement, &a)
	var coded *fault.Error
	if !errors.As(err, &coded) || coded.Code != "harness.session_identity_invalid" {
		t.Fatalf("cross-role discovery: %v", err)
	}
}

func TestLaunchEventFailureRollsBackCreditProbeBinding(t *testing.T) {
	e, w, run, root := executor(t)
	ctx := context.Background()
	// Existing human authorization is a fixture; the execution interface may
	// bind it, but must never mint another recovery probe.
	for _, statement := range []string{
		`INSERT INTO harness_limits(harness_type,reason,restriction_id,probe_id) VALUES('claude','credits_exhausted','restriction','probe')`,
		`INSERT INTO credit_probes(id,harness,restriction_id,run_id,status) VALUES('probe','claude','restriction','run-141','reserved')`,
		`CREATE TRIGGER reject_launch_event BEFORE INSERT ON events WHEN NEW.type='phase.attempt_started' BEGIN SELECT RAISE(ABORT,'event unavailable'); END`,
	} {
		if _, err := e.DB.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	e.Runner = &rejectRunner{Runner: e.Runner, before: func() { t.Fatal("process started before atomic launch commit") }}
	_, a, err := launch(t, e, run, root, config.Role{Agent: "claude"}, 1, "exit 0")
	if err == nil || a.ID != "" {
		t.Fatalf("launch commit should fail: %+v %v", a, err)
	}
	current, err := w.Get(ctx, run.ID)
	if err != nil || current.Implementer != nil || len(current.CreditProbes) != 1 || current.CreditProbes[0].Status != "reserved" || current.CreditProbes[0].AttemptID != "" {
		t.Fatalf("rollback: %+v %v", current, err)
	}
	if _, err := e.DB.ExecContext(ctx, "DROP TRIGGER reject_launch_event"); err != nil {
		t.Fatal(err)
	}
	e.Runner = &rejectRunner{Runner: e.Runner, before: func() {
		saved, err := w.Get(ctx, run.ID)
		if err != nil || saved.Implementer == nil || saved.CreditProbes[0].Status != "running" || saved.CreditProbes[0].AttemptID == "" {
			t.Fatalf("probe not bound before startup: %+v %v", saved, err)
		}
	}}
	_, _, err = launch(t, e, run, root, config.Role{Agent: "claude"}, 1, "exit 0")
	if err == nil {
		t.Fatal("expected fixture startup error")
	}
}
