package scheduler

import (
	"context"
	"database/sql"
	"time"

	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
)

// commentDelivery pairs a frozen comment with its queue's persistence operations.
// The queue owns identities, SQL and event payloads; this policy owns transport,
// validation, cancellation, terminal state and warning suppression for both
// publication kinds.
type commentDelivery struct {
	runID, repository, body, warningCode string
	number                               int
	finished                             bool
	warnings                             publicationWarnings
	startAttempt                         func(context.Context) error
	saveWarning                          func(context.Context, *sql.Tx, publicationOutcome) (events.Draft, error)
	saveAttemptResult                    func(context.Context, *sql.Tx, publicationOutcome) error
	stopPayload                          func(publicationOutcome) map[string]any
	saveReceipt                          func(context.Context, *sql.Tx, *github.Comment) (events.Draft, error)
}

func (s *Scheduler) publishComments(ctx context.Context, pending []commentDelivery) error {
	// Bound optional network work so unavailable GitHub cannot starve phases.
	publicationCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	publisher, supported := s.deps.GitHub.(ReportPublisher)
	for _, p := range pending {
		if publicationCtx.Err() != nil {
			break
		}
		// The durable attempt must exist before an external write can occur.
		if err := p.startAttempt(ctx); err != nil {
			return err
		}
		var comment *github.Comment
		var cause error
		if !supported {
			cause = &fault.Error{Code: "publication.unsupported"}
		} else {
			comment, cause = publisher.EnsureReportComment(publicationCtx, p.repository, p.number, github.CapCommentBody(p.body))
		}
		if cause == nil && (comment == nil || comment.ID <= 0 || comment.URL == "") {
			cause = &fault.Error{Code: "github.invalid_response"}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if cause == nil {
			if _, err := s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
				return p.saveReceipt(ctx, tx, comment)
			}); err != nil {
				return err
			}
			continue
		}
		outcome := outcomeFor(cause, publicationCtx, p.finished, p.warnings)
		if outcome.Code != p.warningCode {
			if _, err := s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
				return p.saveWarning(ctx, tx, outcome)
			}); err != nil {
				return err
			}
			continue
		}
		if !outcome.Stopped {
			if err := p.saveAttemptResult(ctx, nil, outcome); err != nil {
				return err
			}
			continue
		}
		if _, err := s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
			if err := p.saveAttemptResult(ctx, tx, outcome); err != nil {
				return events.Draft{}, err
			}
			return events.Draft{RunID: p.runID, Type: publicationStoppedEvent, Payload: p.stopPayload(outcome)}, nil
		}); err != nil {
			return err
		}
	}
	return nil
}
