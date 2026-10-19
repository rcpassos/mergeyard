package scheduler

import (
	"context"
	"strings"
	"time"
)

const (
	publicationRetryInitial = time.Minute
	publicationRetryMaximum = time.Hour
	// publicationTimeFormat matches SQLite's strftime('%f') so text ordering
	// stays chronological for old and new rows.
	publicationTimeFormat = "2006-01-02T15:04:05.000Z"
	publicationStopped    = " No further retries will be made."
)

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

// The warning events are advisory display text. When a publication stops, only
// that text is corrected; identity, code and ordering stay unchanged.
func (s *Scheduler) updateVerificationWarning(ctx context.Context, runID, source, code, message string) error {
	query := `UPDATE events SET payload_json=json_set(payload_json,'$.message',?)
		WHERE id=(SELECT id FROM events WHERE run_id=? AND type='verification.warning'
			AND json_extract(payload_json,'$.source_attempt')=?`
	args := []any{message, runID, source}
	if code != "" {
		query += ` AND json_extract(payload_json,'$.code')=?`
		args = append(args, code)
	}
	query += ` ORDER BY id DESC LIMIT 1)`
	_, err := s.db.ExecContext(ctx, query, args...)
	return err
}

func (s *Scheduler) updateReportWarning(ctx context.Context, reportID, message string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE events SET payload_json=json_set(payload_json,'$.message',?)
		WHERE id=(SELECT id FROM events WHERE type='publication.warning'
			AND json_extract(payload_json,'$.report_id')=? ORDER BY id DESC LIMIT 1)`, message, reportID)
	return err
}

func (s *Scheduler) updateAttentionWarning(ctx context.Context, eventID int64, message string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE events SET payload_json=json_set(payload_json,'$.message',?)
		WHERE id=(SELECT id FROM events WHERE type='publication.warning'
			AND json_extract(payload_json,'$.attention_event_id')=? ORDER BY id DESC LIMIT 1)`, message, eventID)
	return err
}
