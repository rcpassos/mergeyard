package scheduler

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/rcpassos/mergeyard/internal/events"
)

const (
	publicationRetryInitial = time.Minute
	publicationRetryMaximum = time.Hour
	// publicationTimeFormat matches SQLite's strftime('%f') so text ordering
	// stays chronological for old and new rows.
	publicationTimeFormat   = "2006-01-02T15:04:05.000Z"
	publicationStopped      = " No further retries will be made."
	publicationStoppedEvent = "publication.stopped"
)

type publicationWarnings struct {
	Deferred string
	Terminal string
	Finished string
}

// publicationOutcome is one delivery result. Failed means GitHub returned a
// result for the attempt; Stopped means this queue will not retry it again.
type publicationOutcome struct {
	Code    string
	Message string
	Failed  bool
	Stopped bool
}

func outcomeFor(cause error, publicationCtx context.Context, finished bool, warnings publicationWarnings) publicationOutcome {
	returned := publicationCtx.Err() == nil
	permanent := persistentPublicationFailure(cause, publicationCtx)
	stopped := permanent || (returned && finished)
	message := warnings.Deferred
	if stopped {
		message = warnings.Terminal
		if !permanent {
			message = warnings.Finished
		}
	}
	return publicationOutcome{Code: publicationCode(cause), Message: message, Failed: returned, Stopped: stopped}
}

type publicationState struct {
	Attempts      int
	LastAttemptAt string
	Finished      bool
	Failed        bool
	Stopped       bool
}

type publicationDisposition int

const (
	publicationReady publicationDisposition = iota
	publicationWait
	publicationStop
)

// disposition decides whether a queue may try now, must wait, or must record a
// stop. Stopped rows are left alone; a finished run stops after a returned
// failure, while an interrupted or never-attempted publication still gets a
// chance after its backoff.
func (p publicationState) disposition(now time.Time) (publicationDisposition, error) {
	if p.Stopped {
		return publicationWait, nil
	}
	if p.Finished && p.Failed {
		return publicationStop, nil
	}
	due, err := publicationDue(p.LastAttemptAt, p.Attempts, now)
	if err != nil {
		return publicationWait, err
	}
	if !due {
		return publicationWait, nil
	}
	return publicationReady, nil
}

func stoppedPublicationWarning(message string) string {
	return strings.TrimSpace(message) + publicationStopped
}

func publicationTimestamp(t time.Time) string {
	return t.UTC().Format(publicationTimeFormat)
}

// publicationBackoff returns the wait after a returned failure. attempts counts
// the failed attempt, so the first retry waits one minute and later retries
// double up to the ceiling.
func publicationBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	delay := publicationRetryInitial
	for i := 1; i < attempts && delay < publicationRetryMaximum; i++ {
		delay *= 2
	}
	if delay > publicationRetryMaximum {
		return publicationRetryMaximum
	}
	return delay
}

// publicationDue reports whether a pending publication may run now. A row with
// no attempt yet is always due; a failed attempt waits for its backoff.
func publicationDue(lastAttempt string, attempts int, now time.Time) (bool, error) {
	if attempts < 1 || lastAttempt == "" {
		return true, nil
	}
	attempted, err := time.Parse(time.RFC3339Nano, lastAttempt)
	if err != nil {
		return false, err
	}
	return !now.Before(attempted.Add(publicationBackoff(attempts))), nil
}

// stopPublication atomically records a stop state and appends the visible stop
// event. Event history stays append-only; an already-open dashboard refreshes
// from this event instead of waiting for a rewrite of an older warning.
func (s *Scheduler) stopPublication(ctx context.Context, runID string, payload map[string]any, change func(*sql.Tx) error) error {
	_, err := s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		if err := change(tx); err != nil {
			return events.Draft{}, err
		}
		return events.Draft{RunID: runID, Type: publicationStoppedEvent, Payload: payload}, nil
	})
	return err
}
