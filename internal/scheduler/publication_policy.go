package scheduler

import (
	"context"
	"time"
)

const (
	publicationRetryInitial = time.Minute
	publicationRetryMaximum = time.Hour
	// publicationTimeFormat matches SQLite's strftime('%f') so text ordering
	// stays chronological for old and new rows.
	publicationTimeFormat = "2006-01-02T15:04:05.000Z"
)

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

// terminalPublicationFailure reports whether a returned failure ends retries
// for this publication. Permanent answers cannot resolve on their own, and a
// finished run has no later reader for the result. A shared-budget expiry or an
// interrupted call returns no delivery result and stays retryable.
func terminalPublicationFailure(cause error, publicationCtx context.Context, finished bool) bool {
	if cause == nil || publicationCtx.Err() != nil {
		return false
	}
	if persistentPublicationFailure(cause, publicationCtx) {
		return true
	}
	return finished
}
