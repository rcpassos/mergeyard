package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/maintenance"
	"github.com/rcpassos/mergeyard/internal/review"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

type State string

const (
	Claiming          State = "CLAIMING"
	Preparing         State = "PREPARING"
	Active            State = "ACTIVE"
	WaitingForCI      State = "WAITING_FOR_CI"
	WaitingForHarness State = "WAITING_FOR_HARNESS"
	ReadyToMerge      State = "READY_TO_MERGE"
	Manual            State = "MANUAL"
	NeedsAttention    State = "NEEDS_ATTENTION"
	Failed            State = "FAILED"
	Stopped           State = "STOPPED"
	Completed         State = "COMPLETED"
)

func (s State) Terminal() bool { return s == Failed || s == Stopped || s == Completed }

type Phase string

const (
	Implement Phase = "implement"
	Review    Phase = "review"
	Fix       Phase = "fix"
)

type Trigger string

const (
	IssueClaimed          Trigger = "issue_claimed"
	ClaimSucceeded        Trigger = "claim_succeeded"
	WorktreeReady         Trigger = "worktree_ready"
	ImplementSucceeded    Trigger = "implement_succeeded"
	ReviewApproved        Trigger = "review_approved"
	ReviewChangesRequired Trigger = "review_changes_required"
	ReviewRoundsExhausted Trigger = "review_rounds_exhausted"
	FixSucceeded          Trigger = "fix_succeeded"
	CIPassed              Trigger = "ci_passed"
	CIFailed              Trigger = "ci_failed"
	CIRoundsExhausted     Trigger = "ci_rounds_exhausted"
	PRMerged              Trigger = "pr_merged"
	PRClosedUnmerged      Trigger = "pr_closed_unmerged"
	HarnessLimited        Trigger = "harness_limited"
	HarnessAvailable      Trigger = "harness_available"
	HarnessWaitsExhausted Trigger = "harness_waits_exhausted"
	TakeOver              Trigger = "take_over"
	HandBack              Trigger = "hand_back"
	PhaseBlocked          Trigger = "phase_blocked"
	ResultInvalid         Trigger = "result_invalid"
	PhaseRetriesExhausted Trigger = "phase_retries_exhausted"
	Retry                 Trigger = "retry"
	Stop                  Trigger = "stop"
	InternalFailure       Trigger = "internal_failure"
	OperationFailed       Trigger = "operation_failed"
)

// RunMetadata contains the workflow metadata owned by PR and review components.
type RunMetadata struct {
	PRNumber    int    `json:"pr_number,omitempty"`
	ReviewRound int    `json:"review_round"`
	ApprovedSHA string `json:"approved_sha,omitempty"`
}

// MetadataPatch changes only PR/review metadata on the transitioned run.
// Nil fields preserve their values; an empty ApprovedSHA clears approval.
// IncrementReviewRound advances the persisted round within the transaction.
type MetadataPatch struct {
	Retry                *RetrySnapshot
	PRNumber             *int
	ReviewRound          *int
	ApprovedSHA          *string
	IncrementReviewRound bool
	CI                   *ci.Snapshot
	Fix                  *FixCompletion
	Review               *ReviewCompletion
	ReviewRejection      *ReviewRejection
}

// Run is the persisted lifecycle and workflow metadata snapshot. Worktree and
// agent metadata belongs to the components responsible for those resources.
type Run struct {
	Retries           []RetrySnapshot    `json:"retries,omitempty"`
	SessionRecoveries []SessionRecovery  `json:"session_recoveries,omitempty"`
	Implementer       *ImplementSnapshot `json:"implementer,omitempty"`
	RunMetadata
	Merge            *maintenance.Snapshot `json:"merge,omitempty"`
	CI               *ci.Snapshot          `json:"ci,omitempty"`
	Publications     []review.Publication  `json:"publications,omitempty"`
	ReviewHistory    []review.Snapshot     `json:"review_history,omitempty"`
	FixHistory       []review.FixSnapshot  `json:"fix_history,omitempty"`
	Fix              *review.FixSnapshot   `json:"fix,omitempty"`
	Review           *review.Snapshot      `json:"review,omitempty"`
	ID               string                `json:"id"`
	Repository       string                `json:"repository"`
	IssueNumber      int                   `json:"issue_number"`
	State            State                 `json:"state"`
	Phase            Phase                 `json:"phase,omitempty"`
	CreatedAt        string                `json:"created_at"`
	UpdatedAt        string                `json:"updated_at"`
	CompletedAt      string                `json:"completed_at,omitempty"`
	LastErrorCode    string                `json:"last_error_code,omitempty"`
	LastErrorMessage string                `json:"last_error_message,omitempty"`
}

// Request supplies trigger-specific information, not an arbitrary new state.
type Request struct {
	Trigger     Trigger
	Repository  string
	IssueNumber int
	// NextState is accepted only for Retry after external reconciliation.
	NextState State
	// NextPhase is required for HandBack and retrying an agent-backed state.
	NextPhase Phase
	Failure   *fault.Error
	// Metadata is applied by the workflow in the lifecycle/event transaction.
	Metadata MetadataPatch
}

type Workflow struct {
	db         *sql.DB
	events     *events.Bus
	operations sync.Map
}

// New uses the runtime database and its shared event bus.
func New(db *sql.DB, bus *events.Bus) *Workflow {
	return &Workflow{db: db, events: bus}
}

// Transition is the only lifecycle write boundary for scheduler, UI, and CLI.
func (w *Workflow) Transition(ctx context.Context, id string, request Request) (Run, error) {
	release, err := w.acquireOperation(ctx, id)
	if err != nil {
		return Run{}, err
	}
	defer release()
	var run Run
	_, err = w.events.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		if strings.TrimSpace(id) == "" {
			return events.Draft{}, invalid("Run ID is required")
		}
		current, err := readRun(ctx, tx, id)
		var missing *fault.Error
		if err != nil && !(errors.As(err, &missing) && missing.Code == "internal.run_not_found" && request.Trigger == IssueClaimed) {
			return events.Draft{}, err
		}
		next, eventType, err := destination(current, request)
		if err != nil {
			return events.Draft{}, err
		}
		if request.Metadata.Fix != nil && (current.Phase != Fix || request.Trigger != FixSucceeded) {
			return events.Draft{}, invalid("Fix completion requires fix success transition")
		}
		if request.Metadata.Retry != nil && request.Trigger != Retry && request.Trigger != PRMerged {
			return events.Draft{}, invalid("Retry selection requires a retry or observed merge transition")
		}
		if request.Metadata.ReviewRejection != nil {
			if current.Phase != Review || request.Trigger != OperationFailed || request.Metadata.Review != nil {
				return events.Draft{}, invalid("Review rejection requires a review attention transition")
			}
		}
		if request.Metadata.Review != nil {
			status := request.Metadata.Review.Report.Status
			if current.Phase != Review || (status == "approved" && request.Trigger != ReviewApproved) || (status == "changes_required" && request.Trigger != ReviewChangesRequired && request.Trigger != ReviewRoundsExhausted) || (status != "approved" && status != "changes_required") {
				return events.Draft{}, invalid("Review completion and transition disagree")
			}
		}
		if err := request.Metadata.validate(); err != nil {
			return events.Draft{}, err
		}
		if current.ID == "" {
			if strings.TrimSpace(request.Repository) == "" || request.IssueNumber < 1 {
				return events.Draft{}, invalid("A new run requires a repository and a positive issue number")
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO runs (id, repository, issue_number, state) VALUES (?, ?, ?, ?)`,
				id, request.Repository, request.IssueNumber, next.State)
		} else {
			var code, message any
			if next.State == NeedsAttention || next.State == Failed {
				if request.Failure == nil || strings.TrimSpace(request.Failure.Code) == "" || strings.TrimSpace(request.Failure.Message) == "" {
					return events.Draft{}, invalid("Attention and failed runs require an error code and human message")
				}
				code, message = request.Failure.Code, request.Failure.Message
			}
			var phase any
			if next.Phase != "" {
				phase = next.Phase
			}
			_, err = tx.ExecContext(ctx, `UPDATE runs SET state = ?, current_phase = ?,
				updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
				completed_at = CASE WHEN ? IN ('FAILED', 'STOPPED', 'COMPLETED') THEN strftime('%Y-%m-%dT%H:%M:%fZ', 'now') ELSE NULL END,
				last_error_code = ?, last_error_message = ? WHERE id = ?`,
				next.State, phase, next.State, code, message, id)
		}
		if err != nil {
			return events.Draft{}, storageError(err)
		}
		if err := request.Metadata.apply(ctx, tx, id, request.Failure); err != nil {
			return events.Draft{}, &fault.Error{Code: "internal.run_metadata", Message: "Could not update run metadata", Err: err}
		}
		run, err = readRun(ctx, tx, id)
		if err != nil {
			return events.Draft{}, err
		}
		payload := transitionEvent{From: current.State, FromPhase: current.Phase, Trigger: request.Trigger, Run: run}
		return events.Draft{RunID: id, Type: eventType, Payload: payload}, nil
	})
	if err != nil {
		return Run{}, err
	}
	return run, nil
}

// WithRunOperation coordinates external scheduler operations with all lifecycle
// transitions, including takeover and stop. The callback receives a fresh run
// and a context it must pass to Transition to avoid reacquiring its own gate.
// The supplied context is valid only for the duration of the callback.
func (w *Workflow) WithRunOperation(ctx context.Context, id string, operation func(context.Context, Run) error) error {
	release, err := w.acquireOperation(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	run, err := w.Get(ctx, id)
	if err != nil {
		return err
	}
	operationContext, cancel := context.WithCancel(ctx)
	defer cancel()
	return operation(context.WithValue(operationContext, operationContextKey{}, heldOperation{workflow: w, id: id}), run)
}

type operationContextKey struct{}
type heldOperation struct {
	workflow *Workflow
	id       string
}

func (w *Workflow) acquireOperation(ctx context.Context, id string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if held, ok := ctx.Value(operationContextKey{}).(heldOperation); ok && held.workflow == w && held.id == id {
		return func() {}, nil
	}
	value, _ := w.operations.LoadOrStore(id, make(chan struct{}, 1))
	gate := value.(chan struct{})
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (patch MetadataPatch) apply(ctx context.Context, tx *sql.Tx, id string, failure *fault.Error) error {
	if patch.Retry != nil {
		if err := patch.Retry.Save(ctx, tx, id); err != nil {
			return err
		}
		if v := patch.Retry; v.AttemptFrom > 0 {
			// Reconciliation verified these processes absent before selection.
			// Retire interrupted launch records with the new attempt authorization.
			if _, err := tx.ExecContext(ctx, `UPDATE phase_attempts SET status='failed',error='phase.session_missing: Process absent during explicit retry',ended_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE run_id=? AND phase=? AND round=? AND attempt<? AND status='running'`, id, v.NextPhase, v.Round, v.AttemptFrom); err != nil {
				return err
			}
		}
	}
	if patch.CI != nil {
		if err := ci.Save(ctx, tx, id, *patch.CI); err != nil {
			return err
		}
	}
	if patch.Fix != nil {
		if err := patch.Fix.apply(ctx, tx, id, patch); err != nil {
			return err
		}
	}
	if patch.ReviewRejection != nil {
		if err := patch.ReviewRejection.apply(ctx, tx, id, failure); err != nil {
			return err
		}
	}
	if patch.Review != nil {
		if err := patch.Review.apply(ctx, tx, id, patch); err != nil {
			return err
		}
	}
	if patch.PRNumber == nil && patch.ReviewRound == nil && patch.ApprovedSHA == nil && !patch.IncrementReviewRound {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE runs SET pr_number = COALESCE(?, pr_number),
		review_round = CASE WHEN ? THEN review_round + 1 ELSE COALESCE(?, review_round) END,
		approved_sha = COALESCE(?, approved_sha) WHERE id = ?`,
		patch.PRNumber, patch.IncrementReviewRound, patch.ReviewRound, patch.ApprovedSHA, id)
	return err
}

func (patch MetadataPatch) validate() error {
	if patch.PRNumber != nil && *patch.PRNumber < 1 {
		return invalid("PR number must be positive")
	}
	if patch.ReviewRound != nil {
		if *patch.ReviewRound < 0 {
			return invalid("Review round must be nonnegative")
		}
		if patch.IncrementReviewRound {
			return invalid("Metadata must either set or increment the review round")
		}
	}
	return nil
}

type transitionEvent struct {
	From      State   `json:"from,omitempty"`
	FromPhase Phase   `json:"from_phase,omitempty"`
	Trigger   Trigger `json:"trigger"`
	Run       Run     `json:"run"`
}

type rule struct {
	from      State
	phase     Phase
	trigger   Trigger
	to        State
	nextPhase Phase
	event     string
}

// Empty nextPhase preserves the current phase for waiting and history.
var rules = []rule{
	{"", "", IssueClaimed, Claiming, "", "run.claimed"},
	{Claiming, "", ClaimSucceeded, Preparing, "", "run.preparing"},
	{Preparing, "", WorktreeReady, Active, Implement, "phase.started"},
	{Active, Implement, ImplementSucceeded, Active, Review, "pr.created"},
	{Active, Review, ReviewApproved, WaitingForCI, "", "review.completed"},
	{Active, Review, ReviewChangesRequired, Active, Fix, "review.completed"},
	{Active, Review, ReviewRoundsExhausted, NeedsAttention, "", "run.needs_attention"},
	{Active, Fix, FixSucceeded, Active, Review, "fix.completed"},
	{WaitingForCI, "", CIPassed, ReadyToMerge, "", "ci.updated"},
	{WaitingForCI, "", CIFailed, Active, Fix, "ci.updated"},
	{WaitingForCI, "", CIRoundsExhausted, NeedsAttention, "", "run.needs_attention"},
	{ReadyToMerge, "", PRMerged, Completed, "", "run.completed"},
	{ReadyToMerge, "", PRClosedUnmerged, NeedsAttention, "", "run.needs_attention"},
	{Active, "", HarnessLimited, WaitingForHarness, "", "run.waiting_for_harness"},
	{WaitingForHarness, "", HarnessAvailable, Active, "", "phase.started"},
	{WaitingForHarness, "", HarnessWaitsExhausted, NeedsAttention, "", "run.needs_attention"},
	{Active, "", PhaseBlocked, NeedsAttention, "", "run.needs_attention"},
	{Active, "", ResultInvalid, NeedsAttention, "", "run.needs_attention"},
	{Active, "", PhaseRetriesExhausted, NeedsAttention, "", "run.needs_attention"},
}

// Reconciliation emits the destination's lifecycle event; the payload retains
// Trigger=Retry so consumers can distinguish recovery from normal progression.
var reconciledEvents = map[State]string{
	Claiming:          "run.claimed",
	Preparing:         "run.preparing",
	Active:            "phase.started",
	WaitingForCI:      "ci.updated",
	WaitingForHarness: "run.waiting_for_harness",
	ReadyToMerge:      "pr.ready_for_review",
	Completed:         "run.completed",
}

func destination(current Run, request Request) (Run, string, error) {
	if (request.NextState != "" && request.Trigger != Retry) ||
		(request.NextPhase != "" && request.Trigger != Retry && request.Trigger != HandBack) {
		return Run{}, "", invalid("This trigger does not accept a destination override")
	}
	if current.ID != "" {
		next := current
		switch request.Trigger {
		case TakeOver:
			if !current.State.Terminal() {
				next.State = Manual
				return next, "run.manual", nil
			}
		case PRMerged:
			if !current.State.Terminal() && current.PRNumber > 0 {
				next.State = Completed
				return next, "run.completed", nil
			}
		case Stop:
			if !current.State.Terminal() {
				next.State = Stopped
				return next, "run.stopped", nil
			}
		case OperationFailed:
			if !current.State.Terminal() {
				next.State = NeedsAttention
				return next, "run.needs_attention", nil
			}
		case InternalFailure:
			if !current.State.Terminal() {
				next.State = Failed
				return next, "run.failed", nil
			}
		case HandBack:
			if current.State == Manual && (request.NextPhase == Implement || request.NextPhase == Review) {
				next.State, next.Phase = Active, request.NextPhase
				return next, "run.handed_back", nil
			}
		case Retry:
			if current.State == NeedsAttention || current.State == Failed {
				if eventType, ok := reconciledEvents[request.NextState]; ok {
					if request.NextPhase != "" && !validPhase(request.NextPhase) {
						return Run{}, "", invalid("Reconciliation supplied an unknown phase")
					}
					if (request.NextState == Active || request.NextState == WaitingForHarness) && !validPhase(request.NextPhase) {
						return Run{}, "", invalid("Reconciliation must choose an agent phase")
					}
					next.State, next.Phase = request.NextState, request.NextPhase
					return next, eventType, nil
				}
			}
		}
	}
	// Recovery actions above must remain available even if persisted phase data
	// is malformed. Normal progression still requires a valid agent phase.
	if (current.State == Active || current.State == WaitingForHarness) && !validPhase(current.Phase) {
		return Run{}, "", invalid("An active or harness-waiting run must have an agent phase")
	}
	for _, r := range rules {
		if current.State == r.from && (r.phase == "" || current.Phase == r.phase) && request.Trigger == r.trigger {
			next := current
			next.State = r.to
			if r.nextPhase != "" {
				next.Phase = r.nextPhase
			}
			return next, r.event, nil
		}
	}
	return Run{}, "", invalid(fmt.Sprintf("Cannot apply %s to %s/%s", request.Trigger, current.State, current.Phase))
}

func validPhase(phase Phase) bool { return phase == Implement || phase == Review || phase == Fix }

func invalid(message string) error {
	return &fault.Error{Code: "internal.invalid_transition", Message: message}
}

func storageError(err error) error {
	var constraint *sqlite.Error
	if errors.As(err, &constraint) && constraint.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE {
		return &fault.Error{Code: "internal.run_conflict", Message: "Issue already has an active run", Err: err}
	}
	return &fault.Error{Code: "internal.run_store", Message: "Could not persist or read the run", Err: err}
}

func (w *Workflow) Get(ctx context.Context, id string) (Run, error) {
	return readRun(ctx, w.db, id)
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readRun(ctx context.Context, db queryer, id string) (Run, error) {
	var run Run
	err := db.QueryRowContext(ctx, `SELECT id, repository, issue_number, state, COALESCE(current_phase, ''),
		created_at, updated_at, COALESCE(completed_at, ''), COALESCE(last_error_code, ''), COALESCE(last_error_message, ''),
		COALESCE(pr_number, 0), review_round, COALESCE(approved_sha, '')
		FROM runs WHERE id = ?`, id).Scan(&run.ID, &run.Repository, &run.IssueNumber, &run.State, &run.Phase,
		&run.CreatedAt, &run.UpdatedAt, &run.CompletedAt, &run.LastErrorCode, &run.LastErrorMessage,
		&run.PRNumber, &run.ReviewRound, &run.ApprovedSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, &fault.Error{Code: "internal.run_not_found", Message: "Run does not exist", Path: id, Err: err}
	}
	if err != nil {
		return Run{}, storageError(err)
	}
	run.Merge, err = maintenance.Load(ctx, db, id)
	if err != nil {
		return Run{}, storageError(err)
	}
	run.Retries, err = LoadRetries(ctx, db, id)
	if err != nil {
		return Run{}, storageError(err)
	}
	run.CI, err = ci.Load(ctx, db, id)
	if err != nil {
		return Run{}, storageError(err)
	}
	run.SessionRecoveries, err = LoadSessionRecoveries(ctx, db, id)
	if err != nil {
		return Run{}, storageError(err)
	}
	run.Implementer, err = LoadImplementSnapshot(ctx, db, id)
	if err != nil {
		return Run{}, storageError(err)
	}
	run.FixHistory, err = review.LoadFixHistory(ctx, db, id)
	if err != nil {
		return Run{}, storageError(err)
	}
	if len(run.FixHistory) > 0 {
		run.Fix = &run.FixHistory[len(run.FixHistory)-1]
	}
	run.Publications, err = review.LoadPublications(ctx, db, id)
	if err != nil {
		return Run{}, storageError(err)
	}
	run.ReviewHistory, err = review.LoadHistory(ctx, db, id)
	if err != nil {
		return Run{}, storageError(err)
	}
	run.Review, err = review.LoadSnapshot(ctx, db, id)
	if err != nil {
		return Run{}, storageError(err)
	}
	return run, nil
}
