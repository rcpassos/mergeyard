package scheduler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestStalledVerificationPublicationReleasesReconciliationAndRetries(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {

		flow := testkit.NewScenario(t)
		_, rt, api, _, cfg, r := flow.Parts()
		cfg.PRComments = false
		offline := &testkit.EvidenceGitHub{BranchGitHub: testkit.BranchGitHub{FakeGitHub: api}, Offline: true}
		s, err := flow.New(cfg, scheduler.Dependencies{GitHub: offline, Runner: r})
		if err != nil {
			t.Fatal(err)
		}
		run := flow.Until(s, workflow.WaitingForCI, workflow.Review)
		// Age the deferred attempt so the stalled write is exercised now.
		if _, err := rt.DB.Exec(`UPDATE verification_attempts SET last_attempt_at=? WHERE run_id=?`, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), run.ID); err != nil {
			t.Fatal(err)
		}
		stalled := &testkit.StalledEvidenceGitHub{BranchGitHub: testkit.BranchGitHub{FakeGitHub: api}}
		s, err = flow.New(cfg, scheduler.Dependencies{GitHub: stalled, Runner: r})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		started := time.Now()
		if _, err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if !stalled.Bounded || time.Since(started) > 7*time.Second || ctx.Err() != nil {
			t.Fatal("optional publication was not bounded independently of caller")
		}
		history, err := rt.Events.History(ctx, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		warning := false
		for _, e := range history {
			if e.RunID == run.ID && e.Type == "verification.warning" {
				warning = true
			}
		}
		if !warning {
			t.Fatal("publication timeout warning was not saved")
		}
		// The budget expiry stays retryable but backs off; age the attempt so the
		// healthy scheduler can retry it immediately.
		if _, err := rt.DB.Exec(`UPDATE verification_attempts SET last_attempt_at=? WHERE run_id=?`, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), run.ID); err != nil {
			t.Fatal(err)
		}
		healthy := &testkit.EvidenceGitHub{BranchGitHub: testkit.BranchGitHub{FakeGitHub: api}}
		s, err = flow.New(cfg, scheduler.Dependencies{GitHub: healthy, Runner: r})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if healthy.Updates != 1 {
			t.Fatal("timed out evidence was not retained for retry")
		}
		if _, err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if healthy.Updates != 1 {
			t.Fatal("successful retry receipt was not saved")
		}

	})
}

func TestFailingVerificationPublicationKeepsAttemptsBounded(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	s, rt, publisher, cfg := testkit.FailingVerificationFlowAt(t, "github.unavailable", func() time.Time { return now })
	ctx := context.Background()
	// MANUAL keeps the run open so transient failures retry after backoff,
	// like a live run whose PR updates keep failing while it waits for a human.
	testkit.SeedVerificationRun(t, rt, "probe-manual", "MANUAL", 7, 101)
	baseline := testkit.CountEvents(t, rt, `SELECT count(*) FROM events`)
	growth := []int{}
	for i := range 3 {
		if _, err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		growth = append(growth, testkit.CountEvents(t, rt, `SELECT count(*) FROM events`))
		if i == 0 {
			now = now.Add(time.Minute + time.Second)
		} else if i == 1 {
			now = now.Add(2*time.Minute + time.Second)
		}
	}
	if publisher.Updates != 3 {
		t.Fatalf("live run publication attempts = %d, want one per backoff window", publisher.Updates)
	}
	if attempts := testkit.CountEvents(t, rt, `SELECT count(*) FROM events WHERE type='verification.attempted'`); attempts != 0 {
		t.Fatalf("publication attempts saved %d event rows", attempts)
	}
	if growth[0] != baseline+1 || growth[1] != growth[0] || growth[2] != growth[0] {
		t.Fatalf("event rows kept growing: baseline %d, after reconciliations %v", baseline, growth)
	}
	if warnings := testkit.CountEvents(t, rt, `SELECT count(*) FROM events WHERE run_id=? AND type='verification.warning'`, "probe-manual"); warnings != 1 {
		t.Fatalf("warnings = %d, want one per source and code", warnings)
	}
	var warning string
	if err := rt.DB.QueryRow(`SELECT json_extract(payload_json,'$.message') FROM events WHERE run_id=? AND type='verification.warning'`, "probe-manual").Scan(&warning); err != nil || !strings.Contains(warning, "deferred") {
		t.Fatalf("live-run warning = %q, %v", warning, err)
	}
	var attempts int
	if err := rt.DB.QueryRow(`SELECT attempts FROM verification_attempts WHERE run_id=?`, "probe-manual").Scan(&attempts); err != nil || attempts != 3 {
		t.Fatalf("durable attempt record = %d, %v", attempts, err)
	}
	server, err := web.NewDashboard(rt.Events, rt.Scheduler, web.DashboardOptions{DB: rt.DB, Workspace: rt.Workspace, Scheduler: s, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, testkit.DashboardRequest(t, server, http.MethodGet, "http://127.0.0.1:7331/runs/probe-manual", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "verification.warning") || !strings.Contains(response.Body.String(), "deferred") {
		t.Fatalf("run page lost the publication warning: %d", response.Code)
	}
}

func TestFinishedRunStopsRetryingUnresolvableVerificationFailure(t *testing.T) {
	t.Parallel()

	s, rt, publisher, _ := testkit.FailingVerificationFlow(t, "github.not_found")
	ctx := context.Background()
	testkit.SeedVerificationRun(t, rt, "probe-stopped", "STOPPED", 7, 101)
	baseline := testkit.CountEvents(t, rt, `SELECT count(*) FROM events`)
	growth := []int{}
	for range 3 {
		if _, err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		growth = append(growth, testkit.CountEvents(t, rt, `SELECT count(*) FROM events`))
	}
	if publisher.Updates != 1 {
		t.Fatalf("finished run was retried %d times, want a single delivery attempt", publisher.Updates)
	}
	if growth[0] != baseline+1 || growth[1] != growth[0] || growth[2] != growth[0] {
		t.Fatalf("finished run kept growing event rows: baseline %d, after reconciliations %v", baseline, growth)
	}
	if warnings := testkit.CountEvents(t, rt, `SELECT count(*) FROM events WHERE run_id=? AND type='verification.warning'`, "probe-stopped"); warnings != 1 {
		t.Fatalf("warnings = %d, want one per source and code", warnings)
	}
	var warning string
	if err := rt.DB.QueryRow(`SELECT json_extract(payload_json,'$.message') FROM events WHERE run_id=? AND type='verification.warning'`, "probe-stopped").Scan(&warning); err != nil || !strings.Contains(warning, "cannot update this PR") {
		t.Fatalf("finished-run warning = %q, %v", warning, err)
	}
	var failed bool
	if err := rt.DB.QueryRow(`SELECT failed FROM verification_attempts WHERE run_id=?`, "probe-stopped").Scan(&failed); err != nil || !failed {
		t.Fatalf("unresolvable failure not recorded: %v, %v", failed, err)
	}
	if attempts := testkit.CountEvents(t, rt, `SELECT count(*) FROM events WHERE type='verification.attempted'`); attempts != 0 {
		t.Fatalf("publication attempts saved %d event rows", attempts)
	}
	// A warning saved by an earlier attempt still records the stop without a
	// duplicate warning event.
	if _, err := rt.DB.ExecContext(ctx, `DELETE FROM verification_attempts WHERE run_id=?`, "probe-stopped"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if publisher.Updates != 2 || testkit.CountEvents(t, rt, `SELECT count(*) FROM events WHERE run_id=? AND type='verification.warning'`, "probe-stopped") != 1 {
		t.Fatalf("existing warning duplicated or lost the stop: %d", publisher.Updates)
	}
	if err := rt.DB.QueryRow(`SELECT failed FROM verification_attempts WHERE run_id=?`, "probe-stopped").Scan(&failed); err != nil || !failed {
		t.Fatalf("stop marker lost with an existing warning: %v, %v", failed, err)
	}
}

// A finished run has no later reader for the evidence, so a returned failure
// stops retries even when the failure itself is otherwise transient.
func TestFinishedRunStopsRetryingTransientVerificationFailure(t *testing.T) {
	t.Parallel()

	s, rt, publisher, _ := testkit.FailingVerificationFlow(t, "github.unavailable")
	ctx := context.Background()
	testkit.SeedVerificationRun(t, rt, "probe-transient", "STOPPED", 7, 101)
	baseline := testkit.CountEvents(t, rt, `SELECT count(*) FROM events`)
	growth := []int{}
	for range 3 {
		if _, err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		growth = append(growth, testkit.CountEvents(t, rt, `SELECT count(*) FROM events`))
	}
	if publisher.Updates != 1 {
		t.Fatalf("finished run was retried %d times, want a single delivery attempt", publisher.Updates)
	}
	if growth[0] != baseline+1 || growth[1] != growth[0] || growth[2] != growth[0] {
		t.Fatalf("transient failures grew event rows: baseline %d, after reconciliations %v", baseline, growth)
	}
	if warnings := testkit.CountEvents(t, rt, `SELECT count(*) FROM events WHERE run_id=? AND type='verification.warning'`, "probe-transient"); warnings != 1 {
		t.Fatalf("warnings = %d, want one per source and code", warnings)
	}
	var warning string
	if err := rt.DB.QueryRow(`SELECT json_extract(payload_json,'$.message') FROM events WHERE run_id=? AND type='verification.warning'`, "probe-transient").Scan(&warning); err != nil || !strings.Contains(warning, "will not retry") {
		t.Fatalf("transient warning = %q, %v", warning, err)
	}
	var failed bool
	if err := rt.DB.QueryRow(`SELECT failed FROM verification_attempts WHERE run_id=?`, "probe-transient").Scan(&failed); err != nil || !failed {
		t.Fatalf("finished-run failure not recorded as terminal: %v, %v", failed, err)
	}
}

// A finished run whose attempt hits the shared five-second budget keeps its
// evidence pending, so a later healthy attempt still publishes it.
func TestFinishedRunPublishesAfterBudgetTimeoutRecovers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {

		api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {testkit.Ready(7)}}, Prs: map[string]*github.PullRequest{
			"feature": {Number: 101, State: github.Open, Body: "human body"},
		}, Head: func(string) string { return "" }}
		_, rt := testkit.Fixture(t, api)
		cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
		if err != nil {
			t.Fatal(err)
		}
		stalled := &testkit.StalledEvidenceGitHub{BranchGitHub: testkit.BranchGitHub{FakeGitHub: api}}
		now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
		s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: stalled, Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		testkit.SeedVerificationRun(t, rt, "probe-timeout", "STOPPED", 7, 101)
		if _, err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if !stalled.Bounded {
			t.Fatal("publication was not bounded by the shared budget")
		}
		var failed bool
		if err := rt.DB.QueryRow(`SELECT failed FROM verification_attempts WHERE run_id=?`, "probe-timeout").Scan(&failed); err != nil || failed {
			t.Fatalf("budget timeout marked the run stopped: %v, %v", failed, err)
		}
		now = now.Add(time.Minute + time.Second)
		healthy := &testkit.EvidenceGitHub{BranchGitHub: testkit.BranchGitHub{FakeGitHub: api}}
		s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: healthy, Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if healthy.Updates != 1 || !strings.Contains(api.Prs["feature"].Body, "Valid evidence") {
			t.Fatalf("timed-out finished-run evidence was not published after recovery: %d", healthy.Updates)
		}

	})
}

// A recorded attempt whose call never returned (crash, kill) must not stop a
// finished run: only a returned failure is a delivery result.
func TestInterruptedVerificationAttemptStaysRetryableForFinishedRun(t *testing.T) {
	t.Parallel()

	s, rt, publisher, _ := testkit.FailingVerificationFlow(t, "github.not_found")
	ctx := context.Background()
	testkit.SeedVerificationRun(t, rt, "probe-interrupted", "STOPPED", 7, 101)
	if _, err := rt.DB.ExecContext(ctx, `INSERT INTO verification_attempts(run_id,source_attempt,attempts,last_attempt_at,failed)
		VALUES(?,?,1,'2000-01-01T00:00:00Z',0)`, "probe-interrupted", "probe-interrupted-attempt"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if publisher.Updates != 1 {
		t.Fatalf("interrupted attempt blocked retry: %d calls", publisher.Updates)
	}
	var failed bool
	if err := rt.DB.QueryRow(`SELECT failed FROM verification_attempts WHERE run_id=?`, "probe-interrupted").Scan(&failed); err != nil || !failed {
		t.Fatalf("returned failure not recorded: %v, %v", failed, err)
	}
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if publisher.Updates != 1 {
		t.Fatalf("finished run retried after the returned failure: %d calls", publisher.Updates)
	}
}

func TestSlowVerificationPublicationDoesNotStarveHealthyPRAfterRestart(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {

		// The budget timeout leaves the first PR retryable but backing off, so the
		// untouched healthy PR is attempted after the restart instead of starving.
		api := &testkit.FakeGitHub{Issues: map[string][]github.Issue{}, Prs: map[string]*github.PullRequest{
			"first": {Number: 101, Body: "human first"}, "second": {Number: 102, Body: "human second"},
		}}
		_, rt := testkit.Fixture(t, api)
		cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
		if err != nil {
			t.Fatal(err)
		}
		cfg.PRComments = false
		client := &testkit.FiniteSlowBodyGitHub{FakeGitHub: api}
		s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: client})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for i, id := range []string{"probe-first", "probe-second"} {
			testkit.SeedVerificationRun(t, rt, id, "STOPPED", i+1, 101+i)
		}
		if _, err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		root := rt.Workspace.Root
		if err := rt.Close(); err != nil {
			t.Fatal(err)
		}
		restarted, err := app.Open(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { restarted.Close() })
		s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(restarted), scheduler.Dependencies{GitHub: client})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		t.Logf("body publication call order: %v", client.Calls)
		if len(client.Calls) != 2 || client.Calls[0] != 101 || client.Calls[1] != 102 {
			t.Fatalf("pending evidence did not let the healthy PR through after restart: %v", client.Calls)
		}
		if !strings.Contains(api.Prs["second"].Body, "Valid evidence") {
			t.Fatal("healthy second PR evidence was not published")
		}

	})
}

func TestVerificationPublicationStopsOnPermanentFailure(t *testing.T) {
	t.Parallel()

	for _, code := range []string{"github.invalid_input", "github.forbidden", "github.not_found"} {
		t.Run(code, func(t *testing.T) {
			s, rt, publisher, _ := testkit.FailingVerificationFlow(t, code)
			ctx := context.Background()
			testkit.SeedVerificationRun(t, rt, "probe-permanent", "MANUAL", 7, 101)
			for range 3 {
				if _, err := s.Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if publisher.Updates != 1 {
				t.Fatalf("permanent failure retried: %d calls", publisher.Updates)
			}
			var failed, stopped bool
			if err := rt.DB.QueryRow(`SELECT failed,stopped FROM verification_attempts WHERE run_id=?`, "probe-permanent").Scan(&failed, &stopped); err != nil || !failed || !stopped {
				t.Fatalf("permanent failure state = %v %v, %v", failed, stopped, err)
			}
			var warning string
			if err := rt.DB.QueryRow(`SELECT json_extract(payload_json,'$.message') FROM events WHERE run_id=? AND type='verification.warning'`, "probe-permanent").Scan(&warning); err != nil || !strings.Contains(warning, "cannot update this PR") {
				t.Fatalf("permanent warning = %q, %v", warning, err)
			}
			if warnings := testkit.CountEvents(t, rt, `SELECT count(*) FROM events WHERE run_id=? AND type='verification.warning'`, "probe-permanent"); warnings != 1 {
				t.Fatalf("warnings = %d, want one", warnings)
			}
		})
	}
}

func TestRunFinishingAfterTransientFailureStopsVerificationPublication(t *testing.T) {
	t.Parallel()

	s, rt, publisher, _ := testkit.FailingVerificationFlow(t, "github.unavailable")
	testkit.SeedVerificationRun(t, rt, "probe-finishes-later", "MANUAL", 7, 101)
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if publisher.Updates != 1 {
		t.Fatalf("verification attempts = %d, want 1", publisher.Updates)
	}
	if _, err := rt.DB.Exec(`UPDATE runs SET state='STOPPED' WHERE id='probe-finishes-later'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if publisher.Updates != 1 {
		t.Fatalf("finished verification retried: %d calls", publisher.Updates)
	}
	var stopped bool
	if err := rt.DB.QueryRow(`SELECT stopped FROM verification_attempts WHERE run_id='probe-finishes-later'`).Scan(&stopped); err != nil || !stopped {
		t.Fatalf("finished verification was not stopped: %v, %v", stopped, err)
	}
	if warnings := testkit.CountEvents(t, rt, `SELECT count(*) FROM events WHERE run_id='probe-finishes-later' AND type='verification.warning'`); warnings != 1 {
		t.Fatalf("warnings = %d, want one", warnings)
	}
	var message string
	if err := rt.DB.QueryRow(`SELECT json_extract(payload_json,'$.message') FROM events WHERE run_id='probe-finishes-later' AND type='verification.warning'`).Scan(&message); err != nil || !strings.Contains(message, "deferred") {
		t.Fatalf("verification warning was rewritten: %q, %v", message, err)
	}
	if err := rt.DB.QueryRow(`SELECT json_extract(payload_json,'$.message') FROM events WHERE run_id='probe-finishes-later' AND type='publication.stopped'`).Scan(&message); err != nil || !strings.Contains(message, "will not retry") || !strings.Contains(message, "No further automatic retries") {
		t.Fatalf("verification stop event = %q, %v", message, err)
	}
}

func TestVerificationPublicationBacksOffRecoverableFailures(t *testing.T) {
	t.Parallel()

	for _, code := range []string{"github.command_failed", "github.not_logged_in", "github.timeout"} {
		t.Run(code, func(t *testing.T) {
			now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
			s, rt, publisher, _ := testkit.FailingVerificationFlowAt(t, code, func() time.Time { return now })
			testkit.SeedVerificationRun(t, rt, "probe-recoverable", "MANUAL", 7, 101)
			for range 2 {
				if _, err := s.Reconcile(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if publisher.Updates != 1 {
				t.Fatalf("recoverable failure retried before backoff: %d calls", publisher.Updates)
			}
			now = now.Add(time.Minute + time.Second)
			if _, err := s.Reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			if publisher.Updates != 2 {
				t.Fatalf("recoverable failure did not retry after backoff: %d calls", publisher.Updates)
			}
			var failed, stopped bool
			if err := rt.DB.QueryRow(`SELECT failed,stopped FROM verification_attempts WHERE run_id=?`, "probe-recoverable").Scan(&failed, &stopped); err != nil || !failed || stopped {
				t.Fatalf("recoverable failure state = %v %v, %v", failed, stopped, err)
			}
		})
	}
}
