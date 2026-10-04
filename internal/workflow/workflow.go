package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
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
)

// Run is the lifecycle view of a persisted run. Other run metadata belongs to
// the components responsible for worktrees, agents, and pull requests.
type Run struct {
	ID               string `json:"id"`
	Repository       string `json:"repository"`
	IssueNumber      int    `json:"issue_number"`
	State            State  `json:"state"`
	Phase            Phase  `json:"phase,omitempty"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
	CompletedAt      string `json:"completed_at,omitempty"`
	LastErrorCode    string `json:"last_error_code,omitempty"`
	LastErrorMessage string `json:"last_error_message,omitempty"`
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
}

type Workflow struct {
	db     *sql.DB
	events *events.Bus
}

// New uses the runtime database and its shared event bus.
func New(db *sql.DB, bus *events.Bus) *Workflow {
	return &Workflow{db: db, events: bus}
}

// Transition is the only lifecycle write boundary for scheduler, UI, and CLI.
func (w *Workflow) Transition(ctx context.Context, id string, request Request) (Run, error) {
	var run Run
	_, err := w.events.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
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
		run, err = readRun(ctx, tx, id)
		payload := transitionEvent{From: current.State, FromPhase: current.Phase, Trigger: request.Trigger, Run: run}
		return events.Draft{RunID: id, Type: eventType, Payload: payload}, err
	})
	if err != nil {
		return Run{}, err
	}
	return run, nil
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
	{Active, "", HarnessLimited, WaitingForHarness, "", "harness.usage_limited"},
	{WaitingForHarness, "", HarnessAvailable, Active, "", "harness.available"},
	{WaitingForHarness, "", HarnessWaitsExhausted, NeedsAttention, "", "run.needs_attention"},
	{Active, "", PhaseBlocked, NeedsAttention, "", "run.needs_attention"},
	{Active, "", ResultInvalid, NeedsAttention, "", "run.needs_attention"},
	{Active, "", PhaseRetriesExhausted, NeedsAttention, "", "run.needs_attention"},
}

func destination(current Run, request Request) (Run, string, error) {
	if (request.NextState != "" && request.Trigger != Retry) ||
		(request.NextPhase != "" && request.Trigger != Retry && request.Trigger != HandBack) {
		return Run{}, "", invalid("This trigger does not accept a destination override")
	}
	if (current.State == Active || current.State == WaitingForHarness) && !validPhase(current.Phase) {
		return Run{}, "", invalid("An active or harness-waiting run must have an agent phase")
	}
	if current.ID != "" {
		next := current
		switch request.Trigger {
		case TakeOver:
			if !current.State.Terminal() {
				next.State = Manual
				return next, "run.manual", nil
			}
		case Stop:
			if current.State != Completed {
				next.State = Stopped
				return next, "run.stopped", nil
			}
		case InternalFailure:
			next.State = Failed
			return next, "run.failed", nil
		case HandBack:
			if current.State == Manual && validPhase(request.NextPhase) {
				next.State, next.Phase = Active, request.NextPhase
				return next, "run.handed_back", nil
			}
		case Retry:
			if current.State == NeedsAttention || current.State == Failed {
				switch request.NextState {
				case Claiming, Preparing, Active, WaitingForCI, WaitingForHarness, ReadyToMerge, Completed:
					if request.NextPhase != "" && !validPhase(request.NextPhase) {
						return Run{}, "", invalid("Reconciliation supplied an unknown phase")
					}
					if (request.NextState == Active || request.NextState == WaitingForHarness) && !validPhase(request.NextPhase) {
						return Run{}, "", invalid("Reconciliation must choose an agent phase")
					}
					next.State, next.Phase = request.NextState, request.NextPhase
					return next, "run.retried", nil
				}
			}
		}
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
	return &fault.Error{Code: "internal.run_store", Message: "Could not persist or read the run", Err: err}
}

func (w *Workflow) Get(ctx context.Context, id string) (Run, error) {
	return readRun(ctx, w.db, id)
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readRun(ctx context.Context, db queryer, id string) (Run, error) {
	var run Run
	err := db.QueryRowContext(ctx, `SELECT id, repository, issue_number, state, COALESCE(current_phase, ''),
		created_at, updated_at, COALESCE(completed_at, ''), COALESCE(last_error_code, ''), COALESCE(last_error_message, '')
		FROM runs WHERE id = ?`, id).Scan(&run.ID, &run.Repository, &run.IssueNumber, &run.State, &run.Phase,
		&run.CreatedAt, &run.UpdatedAt, &run.CompletedAt, &run.LastErrorCode, &run.LastErrorMessage)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, &fault.Error{Code: "internal.run_not_found", Message: "Run does not exist", Path: id, Err: err}
	}
	if err != nil {
		return Run{}, storageError(err)
	}
	return run, nil
}
