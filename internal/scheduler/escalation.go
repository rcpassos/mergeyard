package scheduler

import (
	"context"
	"database/sql"
	"reflect"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/escalation"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type EscalationGit interface {
	EscalationDiff(context.Context, managedgit.Run, string) ([]escalation.FileChange, error)
}

type EscalationGitHub interface {
	UpdateEscalation(context.Context, string, int, escalation.Snapshot) error
}

func (s *Scheduler) saveEscalation(ctx context.Context, run *workflow.Run, next escalation.Snapshot) error {
	if reflect.DeepEqual(run.Escalation, &next) {
		return nil
	}
	_, err := s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		return events.Draft{RunID: run.ID, Type: "escalation.updated", Payload: next}, escalation.Save(ctx, tx, run.ID, next)
	})
	if err == nil {
		run.Escalation = &next
	}
	return err
}

// Evaluation failures are advisory warnings; they never fail the CI gate.
// A pending warning retries after readiness, including after a restart.
func (s *Scheduler) evaluateEscalation(ctx context.Context, repo config.Repository, run *workflow.Run) error {
	_, gitRun, err := s.context(ctx, *run)
	if err == nil && run.Escalation != nil && run.Escalation.BaseSHA == gitRun.BaseSHA && run.Escalation.HeadSHA == run.ApprovedSHA && run.Escalation.EvaluationWarning == "" {
		return nil
	}
	next := escalation.Snapshot{BaseSHA: gitRun.BaseSHA, HeadSHA: run.ApprovedSHA, Reasons: []escalation.Flag{}}
	api, ok := s.deps.Git.(EscalationGit)
	if err != nil || !ok || run.ApprovedSHA == "" {
		next.EvaluationWarning = "Escalation evaluation unavailable; inspect the diff before merging. Will retry."
	} else {
		changes, diffErr := api.EscalationDiff(ctx, gitRun, run.ApprovedSHA)
		if diffErr != nil {
			next.EvaluationWarning = "Escalation evaluation unavailable; inspect the diff before merging. Will retry."
		} else {
			next.Reasons = escalation.Evaluate(repo.Escalation, changes)
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return s.saveEscalation(ctx, run, next)
}

func (s *Scheduler) publishEscalation(ctx context.Context, run *workflow.Run) error {
	if run.Escalation == nil || run.Escalation.EvaluationWarning != "" || run.Escalation.Published || run.Escalation.Stopped {
		return nil
	}
	due, err := publicationDue(run.Escalation.LastAttemptAt, run.Escalation.Attempts, s.deps.Now())
	if err != nil {
		return err
	}
	if !due {
		return nil
	}
	next := *run.Escalation
	next.Attempts++
	next.LastAttemptAt = publicationTimestamp(s.deps.Now())
	next.Failed = false
	if err := s.saveEscalation(ctx, run, next); err != nil {
		return err
	}
	api, ok := s.deps.GitHub.(EscalationGitHub)
	// Bound advisory publication so an unavailable GitHub cannot hold dispatch.
	publicationContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var cause error
	if !ok {
		cause = &fault.Error{Code: "publication.unsupported"}
	} else {
		cause = api.UpdateEscalation(publicationContext, run.Repository, run.PRNumber, next)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	next = *run.Escalation
	if cause != nil {
		returned := publicationContext.Err() == nil
		permanent := persistentPublicationFailure(cause, publicationContext)
		terminal := terminalPublicationFailure(cause, publicationContext, run.State.Terminal())
		next.PublicationWarning = "Escalation reasons are saved locally. PR body publication is deferred."
		if terminal {
			next.PublicationWarning = stoppedPublicationWarning("Escalation reasons are saved locally. PR body publication cannot update this PR.")
			if !permanent {
				next.PublicationWarning = stoppedPublicationWarning("Escalation reasons are saved locally. This run is finished, so PR body publication will not retry.")
			}
		}
		next.Failed = returned
		next.Stopped = terminal
	} else {
		next.Published = true
		next.PublicationWarning = ""
		next.Failed = false
		next.Stopped = false
	}
	return s.saveEscalation(ctx, run, next)
}
