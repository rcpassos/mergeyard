package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
)

// publicationRequeuedEvent records that a human asked for another attempt of
// the publications that had stopped.
const publicationRequeuedEvent = "publication.requeued"

// The kinds of publication a run can hold.
const (
	publicationKindReport       = "report"
	publicationKindAttention    = "attention"
	publicationKindVerification = "verification"
)

// StoppedPublication is one publication that will not be retried, with the
// reason it stopped. It stays visible until a human requeues it.
type StoppedPublication struct {
	Kind    string `json:"kind"`
	Label   string `json:"label"`
	Code    string `json:"code,omitempty"`
	Warning string `json:"warning,omitempty"`
}

// stoppedPublicationKind describes one publication table: how to read its
// stopped rows and how to return them to the queue.
type stoppedPublicationKind struct {
	kind  string
	query string
	reset string
	scan  func(*sql.Rows) (StoppedPublication, error)
}

// stoppedPublicationKinds covers every publication that can stop for good.
var stoppedPublicationKinds = []stoppedPublicationKind{
	{
		kind:  publicationKindReport,
		query: `SELECT phase,round,attempt,warning_code,warning FROM report_publications WHERE run_id=? AND stopped=1 ORDER BY round,CASE phase WHEN 'review' THEN 0 ELSE 1 END,attempt`,
		reset: `UPDATE report_publications SET stopped=0,failed=0,attempts=0,last_attempt_at=NULL,warning_code='',warning='' WHERE run_id=? AND stopped=1`,
		scan:  scanStoppedReport,
	},
	{
		kind:  publicationKindAttention,
		query: `SELECT warning_code,warning FROM attention_publications WHERE run_id=? AND stopped=1 ORDER BY event_id`,
		reset: `UPDATE attention_publications SET stopped=0,failed=0,attempts=0,last_attempt_at=NULL,warning_code='',warning='' WHERE run_id=? AND stopped=1`,
		scan:  stoppedReason("attention comment"),
	},
	{
		kind:  publicationKindVerification,
		query: `SELECT warning_code,warning FROM verification_attempts WHERE run_id=? AND stopped=1`,
		reset: `UPDATE verification_attempts SET stopped=0,failed=0,attempts=0,last_attempt_at='',warning_code='',warning='' WHERE run_id=? AND stopped=1`,
		scan:  stoppedReason("pull request body"),
	},
}

// StoppedPublications returns the run's stopped publications with the reason
// each one stopped. A stopped publication is never retried on its own, so the
// reason is the only account of what went wrong.
func (s *Scheduler) StoppedPublications(ctx context.Context, runID string) ([]StoppedPublication, error) {
	var stopped []StoppedPublication
	for _, kind := range stoppedPublicationKinds {
		rows, err := s.db.QueryContext(ctx, kind.query, runID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			p, err := kind.scan(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			p.Kind = kind.kind
			stopped = append(stopped, p)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return stopped, nil
}

// RequeuePublications returns a run's stopped publications to the pending
// queue. A permanent failure stops a publication for good, and only a human
// knows the cause is fixed, so the operator asks for another attempt instead
// of editing the database. The requeue clears the stop and its reason, resets
// the backoff, and lets the next reconciliation deliver the frozen comment or
// pull request body again. A comment that was already delivered is still
// matched by its marker, so a requeue never duplicates it.
func (s *Scheduler) RequeuePublications(ctx context.Context, runID string) ([]StoppedPublication, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return nil, &fault.Error{Code: "internal.run_not_found", Message: "Run does not exist", Path: runID}
	}
	if _, err := s.workflow.Get(ctx, runID); err != nil {
		return nil, err
	}
	stopped, err := s.StoppedPublications(ctx, runID)
	if err != nil {
		return nil, err
	}
	if len(stopped) == 0 {
		return nil, nil
	}
	_, err = s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		count, err := requeueStoppedPublications(ctx, tx, runID)
		if err != nil {
			return events.Draft{}, err
		}
		return events.Draft{RunID: runID, Type: publicationRequeuedEvent, Payload: map[string]any{"count": count}}, nil
	})
	if err != nil {
		return nil, err
	}
	return stopped, nil
}

// requeueStoppedPublications clears the stop, the reason and the backoff of
// every stopped publication of one run and returns how many it returned to
// the queue. The caller owns the transaction, so a Retry can commit the
// requeue together with its own intent.
func requeueStoppedPublications(ctx context.Context, tx *sql.Tx, runID string) (int, error) {
	count := 0
	for _, kind := range stoppedPublicationKinds {
		result, err := tx.ExecContext(ctx, kind.reset, runID)
		if err != nil {
			return 0, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		count += int(affected)
	}
	return count, nil
}

// stoppedReason reads the reason of a stopped publication that carries no
// further identity.
func stoppedReason(label string) func(*sql.Rows) (StoppedPublication, error) {
	return func(rows *sql.Rows) (StoppedPublication, error) {
		var p StoppedPublication
		err := rows.Scan(&p.Code, &p.Warning)
		p.Label = label
		return p, err
	}
}

// scanStoppedReport reads the report a stopped publication carries.
func scanStoppedReport(rows *sql.Rows) (StoppedPublication, error) {
	var p StoppedPublication
	var phase string
	var round, attempt int
	if err := rows.Scan(&phase, &round, &attempt, &p.Code, &p.Warning); err != nil {
		return p, err
	}
	p.Label = reportPublicationLabel(phase, round, attempt)
	return p, nil
}

// reportPublicationLabel names the report a publication carries.
func reportPublicationLabel(phase string, round, attempt int) string {
	name := "review report"
	if phase == "fix" {
		name = "fix report"
	}
	return fmt.Sprintf("%s · round %d, attempt %d", name, round, attempt)
}
