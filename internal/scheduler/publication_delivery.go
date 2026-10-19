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
	repository, body, warningCode    string
	deferredWarning, terminalWarning string
	finishedWarning                  string
	number                           int
	finished                         bool
	startAttempt                     func(context.Context) error
	saveWarning                      func(context.Context, *sql.Tx, string, string, bool, bool) (events.Draft, error)
	saveAttemptResult                func(context.Context, string, string, bool, bool) error
	saveReceipt                      func(context.Context, *sql.Tx, *github.Comment) (events.Draft, error)
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
		var err error
		if cause != nil {
			code := publicationCode(cause)
			// A shared-budget expiry returns no delivery result; keep the
			// attempt retryable and do not mark the last result as failed.
			returned := publicationCtx.Err() == nil
			permanent := persistentPublicationFailure(cause, publicationCtx)
			terminal := terminalPublicationFailure(cause, publicationCtx, p.finished)
			message := p.deferredWarning
			if terminal {
				message = p.terminalWarning
				if !permanent {
					message = p.finishedWarning
				}
			}
			if code == p.warningCode {
				// The warning is already visible; correct its text if retries stop.
				if err := p.saveAttemptResult(ctx, code, message, returned, terminal); err != nil {
					return err
				}
				continue
			}
			_, err = s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
				return p.saveWarning(ctx, tx, code, message, returned, terminal)
			})
		} else {
			_, err = s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
				return p.saveReceipt(ctx, tx, comment)
			})
		}
		if err != nil {
			return err
		}
	}
	return nil
}
