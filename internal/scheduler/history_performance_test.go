package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/store"
	"github.com/rcpassos/mergeyard/internal/workflow"
	"github.com/rcpassos/mergeyard/internal/workspace"
)

// Run history grows without bound. Identity observation repeats every 250ms, so
// it must never overrun its own ticker; verification publication repeats once
// per tick, which must stay almost entirely free for the phases and lifecycle
// controls that share its lock and the single database connection.
const (
	historyIdentityBudget  = 250 * time.Millisecond
	historyTickBudget      = time.Second
	historySeedRuns        = 2000
	historySeedRounds      = 3
	historySeedCIPolls     = 6
	historyChangesRequired = `{"status":"changes_required"}`
	historyThreadID        = "01a10c5b-38a9-7330-b833-04e365cb5f37"
	historyReviewThreadID  = "01a10c5b-38a9-7330-b833-04e365cb5f38"
	historyFixThreadID     = "01a10c5b-38a9-7330-b833-04e365cb5f39"
	historyRepo            = "owner/repo"
	historyPendingPR       = 90001
)

// historyRepeats is the number of measured rounds per pass. Each round restores
// the durable state the pass changes, and the fastest round is reported, so a
// loaded machine does not turn unrelated scheduler noise into a failure.
const historyRepeats = 3

func TestHistoryKeepsIdentityObservationAndPublicationInsideBudget(t *testing.T) {
	ctx := context.Background()
	db, phases := historyStore(t)
	seedFinishedHistory(t, db, historySeedRuns)
	candidates := seedDiscoverableRuns(t, db, phases)
	candidate := candidates[0]
	s := historyScheduler(t, db)

	observed := historyFastest(t, func() {
		// Discard every identity the previous round saved so the pass observes
		// each phase capture again.
		for _, c := range candidates {
			c.restore(t, db)
		}
		reset(t, db, "DELETE FROM events WHERE type='harness.session_discovered'")
	}, func() error { return s.observeIdentities(ctx) })
	published := historyFastest(t, func() {
		// Discard the receipts the previous round wrote so it publishes the same report.
		reset(t, db, "DELETE FROM events WHERE run_id=? AND type IN ('verification.attempted','verification.published')", candidate.runID)
	}, func() error { return s.publishVerification(ctx) })
	// The tick's own load must skip finished history too, or reconciliation
	// still holds the lock that identity observation and publication wait on.
	reconciled := historyFastest(t, func() {}, func() error {
		_, err := s.tickRuns(ctx)
		return err
	})

	// Narrowing the candidate set must keep observing real discoveries in every
	// phase and round the SQL mirrors from attemptRound and Phase.Role.
	for _, c := range candidates {
		for _, check := range c.identityChecks() {
			var got string
			if err := db.QueryRowContext(ctx, check.query, check.arg).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != c.identity {
				t.Fatalf("%s = %q, want %q", check.label, got, c.identity)
			}
		}
	}
	var receipts int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE run_id=? AND type='verification.published' AND json_extract(payload_json,'$.pr_number')=?`, candidate.runID, historyPendingPR).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if receipts != 1 {
		t.Fatalf("verification receipts = %d, want 1", receipts)
	}
	if observed > historyIdentityBudget || published > historyTickBudget || reconciled > historyTickBudget {
		t.Fatalf("seeded history broke its budget: identity observation %s of %s, verification publication %s of %s, tick load %s of %s", observed, historyIdentityBudget, published, historyTickBudget, reconciled, historyTickBudget)
	}
	var seeded int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM events").Scan(&seeded); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d runs and %d seeded events: identity observation %s of %s, verification publication %s of %s, tick load %s of %s", historySeedRuns, seeded, observed, historyIdentityBudget, published, historyTickBudget, reconciled, historyTickBudget)
}

// historyFastest restores the pass's inputs and times it, reporting the fastest
// round so unrelated scheduler noise cannot turn a fast pass into a failure.
func historyFastest(t *testing.T, restore func(), pass func() error) time.Duration {
	t.Helper()
	fastest := time.Duration(math.MaxInt64)
	for range historyRepeats {
		restore()
		start := time.Now()
		if err := pass(); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed < fastest {
			fastest = elapsed
		}
	}
	return fastest
}

func reset(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}

// historyStore builds the migrated single-connection store the runtime owns.
func historyStore(t *testing.T) (*sql.DB, string) {
	t.Helper()
	root := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, filepath.Join(root, "runs")
}

func historyScheduler(t *testing.T, db *sql.DB) *Scheduler {
	t.Helper()
	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: " + historyRepo + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	home, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bus := events.New(db, nil)
	s, err := New(cfg, Resources{DB: db, Events: bus, Workflow: workflow.New(db, bus), Workspace: home}, Dependencies{GitHub: historyGitHub{}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// historyGitHub answers publication; every other boundary stays unreachable.
type historyGitHub struct{ GitHub }

func (historyGitHub) UpdatePullRequest(context.Context, string, int, github.PullRequestContent) (*github.PullRequest, error) {
	return nil, nil
}

// historyCandidate is one active run whose running current-phase attempt has no
// recorded session identity yet. Discovery must save a capture's identity in the
// run, the attempt, and the phase journal when one exists.
type historyCandidate struct {
	runID     string // runs.id
	attemptID string // running phase_attempts row
	phase     string
	column    string // runs role identity column discovery writes
	journal   string // role journal table, empty for implement
	identity  string // thread id the capture reports
}

// restore clears every identity discovery wrote so the pass observes the
// candidate again, including the role journal masked by the identity chain.
func (c historyCandidate) restore(t *testing.T, db *sql.DB) {
	t.Helper()
	reset(t, db, "UPDATE runs SET "+c.column+"=NULL WHERE id=?", c.runID)
	reset(t, db, "UPDATE phase_attempts SET session_id=NULL WHERE id=?", c.attemptID)
	if c.journal != "" {
		reset(t, db, "UPDATE "+c.journal+" SET session_id='' WHERE attempt_id=?", c.attemptID)
	}
}

func (c historyCandidate) identityChecks() []struct{ label, query, arg string } {
	checks := []struct{ label, query, arg string }{
		{c.phase + " run identity", "SELECT COALESCE(" + c.column + ",'') FROM runs WHERE id=?", c.runID},
		{c.phase + " attempt identity", "SELECT COALESCE(session_id,'') FROM phase_attempts WHERE id=?", c.attemptID},
	}
	if c.journal != "" {
		checks = append(checks, struct{ label, query, arg string }{c.phase + " journal identity", "SELECT COALESCE(session_id,'') FROM " + c.journal + " WHERE attempt_id=?", c.attemptID})
	}
	return checks
}

type historySeeder struct {
	run     *sql.Stmt
	context *sql.Stmt
	attempt *sql.Stmt
	review  *sql.Stmt
	event   *sql.Stmt
	nextID  int64
	tx      *sql.Tx
}

// seedFinishedHistory writes a long tail of merged runs. Each run keeps the
// shape mergeyard actually produces — an implementation, review rounds with
// fixes, polled CI — so the event table grows with real history rather than
// with an artificial per-run loop. Their implement completion already carries a
// publication receipt, so only the separately seeded active run waits for the
// publication pass under test.
func seedFinishedHistory(t *testing.T, db *sql.DB, runs int) {
	t.Helper()
	s := historyStart(t, db)
	defer s.tx.Rollback()
	for i := range runs {
		id := fmt.Sprintf("history-%05d", i)
		session := fmt.Sprintf("history-session-%05d", i)
		if _, err := s.run.ExecContext(t.Context(), id, historyRepo, i+1, 1000+i, session, session); err != nil {
			t.Fatal(err)
		}
		if _, err := s.context.ExecContext(t.Context(), id, fmt.Sprintf(`{"number":%d,"title":"Implement this","state":"open"}`, i+1)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.attempt.ExecContext(t.Context(), id+"-implement", id, string(workflow.Implement), workflow.Implement.Role(), 0, 1, "codex", session); err != nil {
			t.Fatal(err)
		}
		s.record(t, id, "run.claimed", `{}`)
		s.record(t, id, "phase.attempt_started", historyPhase("implement", 1))
		completed := s.record(t, id, "phase.completed", historyReport("implement", 1))
		s.record(t, id, "verification.attempted", historySource(completed))
		s.record(t, id, "verification.published", historySource(completed))
		s.record(t, id, "pr.created", `{}`)
		for round := 1; round <= historySeedRounds; round++ {
			review := fmt.Sprintf("%s-review-%d", id, round)
			if _, err := s.attempt.ExecContext(t.Context(), review, id, string(workflow.Review), workflow.Review.Role(), round, 1, "codex", session); err != nil {
				t.Fatal(err)
			}
			if _, err := s.review.ExecContext(t.Context(), review, session); err != nil {
				t.Fatal(err)
			}
			s.record(t, id, "phase.attempt_started", historyPhase("review", round))
			s.record(t, id, "phase.completed", historyReport("review", round))
			s.record(t, id, "review.completed", historyChangesRequired)
			fix := fmt.Sprintf("%s-fix-%d", id, round)
			if _, err := s.attempt.ExecContext(t.Context(), fix, id, string(workflow.Fix), workflow.Fix.Role(), round, 1, "codex", session); err != nil {
				t.Fatal(err)
			}
			s.record(t, id, "phase.attempt_started", historyPhase("fix", round))
			repaired := s.record(t, id, "phase.completed", historyReport("fix", round))
			s.record(t, id, "verification.attempted", historySource(repaired))
			s.record(t, id, "verification.published", historySource(repaired))
			s.record(t, id, "fix.completed", `{"round":`+strconv.Itoa(round)+`}`)
			for range historySeedCIPolls {
				s.record(t, id, "ci.updated", `{"status":"pending"}`)
			}
		}
		s.record(t, id, "pr.ready_for_review", `{}`)
		s.record(t, id, "run.completed", `{}`)
	}
	if err := s.tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// seedDiscoverableRuns adds one active run per phase whose running
// current-phase attempt has no recorded session identity yet, each alongside a
// capture holding its identity. The candidates keep every phase rule
// Discoverable mirrors from attemptRound and Phase.Role falsifiable: implement
// must match round zero despite a nonzero review round, and it must read the
// implementer identity even while a reviewer conversation is recorded; review
// and fix must match the review round of their phase.
func seedDiscoverableRuns(t *testing.T, db *sql.DB, phases string) []historyCandidate {
	t.Helper()
	return []historyCandidate{
		seedDiscoverable(t, db, phases, discoverable{
			id: "identity-candidate", phase: workflow.Implement, round: 0, reviewRound: 1, issue: 1,
			identity: historyThreadID, prNumber: historyPendingPR, publication: true,
			reviewer: "history-reviewer-conversation",
		}),
		seedDiscoverable(t, db, phases, discoverable{
			id: "identity-review", phase: workflow.Review, round: 2, reviewRound: 2, issue: 2,
			identity: historyReviewThreadID, implementer: "history-implementer-conversation",
		}),
		seedDiscoverable(t, db, phases, discoverable{
			id: "identity-fix", phase: workflow.Fix, round: 1, reviewRound: 1, issue: 3,
			identity: historyFixThreadID, reviewer: "history-reviewer-conversation",
		}),
	}
}

type discoverable struct {
	id          string
	phase       workflow.Phase
	round       int // running attempt round the phase rules require
	reviewRound int // runs.review_round, nonzero to falsify the implement round rule
	issue       int
	identity    string
	prNumber    int
	implementer string // recorded implementer conversation
	reviewer    string // recorded reviewer conversation
	publication bool
}

func seedDiscoverable(t *testing.T, db *sql.DB, phases string, spec discoverable) historyCandidate {
	t.Helper()
	ctx := t.Context()
	attemptID := spec.id + "-attempt"
	dir := filepath.Join(phases, spec.id, fmt.Sprintf("%s-%d-%d", spec.phase, spec.round, 1))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	stream := fmt.Sprintf("{\"type\":\"thread.started\",\"thread_id\":%q}\n", spec.identity)
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(stream), 0600); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(dir, "input.md")
	if err := os.WriteFile(input, []byte("# Task\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO runs (id,repository,issue_number,state,current_phase,review_round,pr_number,implementer_agent,reviewer_agent,implementer_session_id,reviewer_session_id) VALUES (?,?,?,'ACTIVE',?,?,?,'codex','codex',?,?)`, spec.id, historyRepo, spec.issue, string(spec.phase), spec.reviewRound, spec.prNumber, spec.implementer, spec.reviewer); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO phase_attempts (id,run_id,phase,role,round,attempt,agent,status,step,process_session,input_path,session_id) VALUES (?,?,?,?,?,1,'codex','running','',?,?,NULL)`, attemptID, spec.id, string(spec.phase), spec.phase.Role(), spec.round, "identity-"+spec.id, input); err != nil {
		t.Fatal(err)
	}
	journal := ""
	switch spec.phase {
	case workflow.Review:
		journal = "review_attempts"
		if _, err := db.ExecContext(ctx, `INSERT INTO review_attempts (attempt_id,target_sha,diff,snapshot_json,session_id,permission_mode,allowed_tools_json) VALUES (?,'','{}','{}','','default','[]')`, attemptID); err != nil {
			t.Fatal(err)
		}
	case workflow.Fix:
		journal = "fix_attempts"
		if _, err := db.ExecContext(ctx, `INSERT INTO fix_attempts (attempt_id,session_id,target_sha,findings_json,permission_mode,allowed_tools_json) VALUES (?,'','','[]','default','[]')`, attemptID); err != nil {
			t.Fatal(err)
		}
	}
	// Only the implement candidate waits for publication, so only it needs the
	// issue context and the completion events that carry a report.
	if spec.publication {
		if _, err := db.ExecContext(ctx, `INSERT INTO scheduler_runs (run_id,issue_json) VALUES (?,?)`, spec.id, `{"number":1,"title":"Implement this","state":"open"}`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO events (run_id,type,payload_json) VALUES (?,'phase.attempt_started',?)`, spec.id, historyPhase("implement", 1)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO events (run_id,type,payload_json) VALUES (?,'phase.completed',?)`, spec.id, historyReport("implement", 1)); err != nil {
			t.Fatal(err)
		}
	}
	return historyCandidate{runID: spec.id, attemptID: attemptID, phase: string(spec.phase), column: spec.phase.Role() + "_session_id", journal: journal, identity: spec.identity}
}

func historyStart(t *testing.T, db *sql.DB) *historySeeder {
	t.Helper()
	ctx := t.Context()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := &historySeeder{tx: tx}
	if s.run, err = tx.PrepareContext(ctx, `INSERT INTO runs (id,repository,issue_number,state,current_phase,pr_number,review_round,implementer_agent,implementer_session_id,reviewer_agent,reviewer_session_id) VALUES (?,?,?,'COMPLETED','review',?,1,'codex',?,'codex',?)`); err != nil {
		t.Fatal(err)
	}
	if s.context, err = tx.PrepareContext(ctx, `INSERT INTO scheduler_runs (run_id,issue_json) VALUES (?,?)`); err != nil {
		t.Fatal(err)
	}
	if s.attempt, err = tx.PrepareContext(ctx, `INSERT INTO phase_attempts (id,run_id,phase,role,round,attempt,agent,status,step,session_id) VALUES (?,?,?,?,?,?,'codex','succeeded','',?)`); err != nil {
		t.Fatal(err)
	}
	if s.review, err = tx.PrepareContext(ctx, `INSERT INTO review_attempts (attempt_id,target_sha,diff,snapshot_json,session_id,permission_mode,allowed_tools_json) VALUES (?,'','{}','{}',?,'default','[]')`); err != nil {
		t.Fatal(err)
	}
	// Event IDs are assigned here so publication receipts can reference the
	// completion they answer without depending on RETURNING on a prepared write.
	if s.event, err = tx.PrepareContext(ctx, `INSERT INTO events (id,run_id,type,payload_json) VALUES (?,?,?,?)`); err != nil {
		t.Fatal(err)
	}
	return s
}

// record appends one seeded event and returns its durable ID.
func (s *historySeeder) record(t *testing.T, runID, eventType, payload string) int64 {
	t.Helper()
	s.nextID++
	if _, err := s.event.ExecContext(t.Context(), s.nextID, runID, eventType, payload); err != nil {
		t.Fatal(err)
	}
	return s.nextID
}

func historyPhase(phase string, attempt int) string {
	return fmt.Sprintf(`{"phase":%q,"attempt":%d,"agent":"codex"}`, phase, attempt)
}

func historyReport(phase string, attempt int) string {
	evidence := "[]"
	if phase != "review" {
		evidence = `[{"check":"go test ./...","status":"passed"}]`
	}
	return fmt.Sprintf(`{"phase":%q,"attempt":%d,"result":{"schema_version":3,"status":"success","summary":"Implemented the change","verification":%s}}`, phase, attempt, evidence)
}

func historySource(event int64) string {
	return fmt.Sprintf(`{"source_event":%d}`, event)
}
