package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/workflow"
	"time"
)

// CIGitHub is the commit evidence and journaled readiness boundary.
type CIGitHub interface {
	CheckEvidence(context.Context, string, string, string) (ci.Evidence, error)
	MarkReady(context.Context, string, int) error
}

func (s *Scheduler) saveCI(ctx context.Context, run workflow.Run, wait ci.Snapshot, event string) error {
	if run.CI != nil {
		old, _ := json.Marshal(run.CI)
		next, _ := json.Marshal(wait)
		if string(old) == string(next) {
			return nil
		}
	}
	_, err := s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		return events.Draft{RunID: run.ID, Type: event, Payload: wait}, ci.Save(ctx, tx, run.ID, wait)
	})
	return err
}
func (s *Scheduler) ciAttention(ctx context.Context, repo config.Repository, run workflow.Run, wait ci.Snapshot, code, message string) error {
	approved := run.ApprovedSHA
	if code == "ci.head_changed" || code == "ci.approval_invalid" {
		approved = ""
	}
	_, err := s.workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.OperationFailed, Failure: &fault.Error{Code: code, Message: message}, Metadata: workflow.MetadataPatch{CI: &wait, ApprovedSHA: &approved}})
	if err != nil {
		return err
	}
	return s.attentionLabels(ctx, repo, run)
}
func (s *Scheduler) waitCI(ctx context.Context, repo config.Repository, run workflow.Run) error {
	wait := run.CI
	if wait == nil {
		// Upgrade runs approved before CI waits existed, once, without resetting later.
		now := s.deps.Now().UTC()
		wait = &ci.Snapshot{SHA: run.ApprovedSHA, StartedAt: now, Deadline: now.Add(s.cfg.CITimeout)}
		if err := s.saveCI(ctx, run, *wait, "ci.updated"); err != nil {
			return err
		}
	}
	v := *wait
	fail := func(code, message string) error { return s.ciAttention(ctx, repo, run, v, code, message) }
	if run.ApprovedSHA == "" || run.ApprovedSHA != v.SHA || run.Review == nil || !run.Review.Accepted || run.Review.TargetSHA != run.ApprovedSHA || run.Review.Report == nil || run.Review.Report.Status != "approved" {
		return fail("ci.approval_invalid", "CI wait lacks an accepted review for the approved commit; inspect preserved work")
	}
	now := s.deps.Now().UTC()
	unknown := func(err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		v.QueryError = err.Error()
		if !s.deps.Now().UTC().Before(v.Deadline) {
			return fail("ci.wait_timeout", "CI wait deadline expired with unknown evidence: "+v.QueryError)
		}
		return s.saveCI(ctx, run, v, "ci.updated")
	}
	_, gitRun, err := s.context(ctx, run)
	if err != nil {
		return unknown(err)
	}
	pr, err := s.deps.GitHub.GetPullRequest(ctx, repo.Repo, run.PRNumber)
	if err != nil {
		v.CurrentHead = ""
		return unknown(err)
	}
	if pr == nil {
		return unknown(&fault.Error{Code: "github.invalid_response", Message: "Missing PR"})
	}
	v.CurrentHead = pr.Head.SHA
	if pr.State == github.Closed {
		if pr.Merged {
			return s.saveCI(ctx, run, v, "ci.updated")
		}
		return fail("ci.pr_closed", "Pull request closed without merging; preserved work requires attention")
	}
	if !reviewHeadMatches(pr, repo, run, gitRun, run.ApprovedSHA) {
		return fail("ci.head_changed", "PR head differs from the review target and approved commit; approval invalidated. Inspect preserved work before explicit retry")
	}
	api, ok := s.deps.GitHub.(CIGitHub)
	if !ok {
		return unknown(&fault.Error{Code: "github.ci_unsupported", Message: "GitHub adapter cannot establish check requirements"})
	}
	base := pr.Base.Ref
	if base == "" {
		return unknown(&fault.Error{Code: "github.invalid_response", Message: "PR base branch is missing; requirements remain unknown"})
	}
	evidence, err := api.CheckEvidence(ctx, repo.Repo, run.ApprovedSHA, base)
	v.Evidence = evidence
	v.Evidence.Gate()
	if err != nil {
		return unknown(err)
	}
	v.QueryError = ""
	now = s.deps.Now().UTC()
	passed, attention := v.Evidence.Gate()
	if v.Evidence.SHA != run.ApprovedSHA {
		passed = false
		return unknown(&fault.Error{Code: "ci.evidence_stale", Message: "Checks were queried for another commit"})
	}
	if attention != "" {
		message := "CI failed or timed out. Check diagnostics are saved; automatic CI repair is unavailable until the dependent repair slice lands"
		if attention == "ci.action_required" {
			message = "CI was canceled or requires action; inspect the saved check outcomes and links"
		}
		return fail(attention, message)
	}
	if !now.Before(v.Deadline) {
		return fail("ci.wait_timeout", "CI wait deadline expired; inspect saved check evidence. No code fix was launched")
	}
	if len(v.Evidence.Checks) == 0 && len(v.Evidence.Required) == 0 {
		passed = passed && !now.Before(v.StartedAt.Add(2*time.Minute))
	}
	if !passed {
		return s.saveCI(ctx, run, v, "ci.updated")
	}
	// Re-read after check queries to catch a push or closure during observation.
	fresh, err := s.deps.GitHub.GetPullRequest(ctx, repo.Repo, run.PRNumber)
	if err != nil {
		v.CurrentHead = ""
		return unknown(err)
	}
	if fresh == nil {
		return unknown(&fault.Error{Code: "github.invalid_response", Message: "Missing PR before readiness"})
	}
	v.CurrentHead = fresh.Head.SHA
	if !reviewHeadMatches(fresh, repo, run, gitRun, run.ApprovedSHA) {
		return fail("ci.head_changed", "PR changed while checking CI; approval invalidated before readiness")
	}
	if fresh.Base.Ref != base {
		return unknown(&fault.Error{Code: "ci.base_changed", Message: "PR base changed while querying requirements"})
	}
	if !s.deps.Now().UTC().Before(v.Deadline) {
		return fail("ci.wait_timeout", "CI wait deadline expired before readiness; no code fix was launched")
	}
	if fresh.Draft {
		if v.ReadyStarted {
			return fail("ci.readiness_ambiguous", "A prior mark-ready write has uncertain outcome and PR is still draft. Inspect GitHub before explicit retry; write was not replayed")
		}
		v.ReadyStarted = true
		if err := s.saveCI(ctx, run, v, "pr.readiness_started"); err != nil {
			return err
		}
		writeErr := api.MarkReady(ctx, repo.Repo, run.PRNumber)
		// Even a successful write is observed again: do not trust a stale response.
		observed, err := s.deps.GitHub.GetPullRequest(ctx, repo.Repo, run.PRNumber)
		if err != nil {
			return unknown(err)
		}
		if observed == nil {
			return unknown(&fault.Error{Code: "github.invalid_response", Message: "Missing PR after readiness mutation"})
		}
		v.CurrentHead = observed.Head.SHA
		if !reviewHeadMatches(observed, repo, run, gitRun, run.ApprovedSHA) {
			return fail("ci.head_changed", "PR changed during mark-ready; approval invalidated")
		}
		if observed.Base.Ref != base {
			return fail("ci.readiness_ambiguous", "PR base changed during readiness; requirements must be inspected before explicit retry")
		}
		if observed.Draft {
			message := "Mark-ready did not establish a normal PR; inspect before explicit retry"
			if writeErr != nil {
				message += " — " + writeErr.Error()
			}
			return fail("ci.readiness_ambiguous", message)
		}
	}
	if !s.deps.Now().UTC().Before(v.Deadline) {
		return fail("ci.wait_timeout", "CI wait deadline expired before readiness acknowledgement; inspect GitHub and saved evidence")
	}
	_, err = s.workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.CIPassed, Metadata: workflow.MetadataPatch{CI: &v}})
	return err
}
func (s *Scheduler) observeReady(ctx context.Context, repo config.Repository, run workflow.Run) error {
	pr, err := s.deps.GitHub.GetPullRequest(ctx, repo.Repo, run.PRNumber)
	if err != nil {
		return err
	}
	if pr == nil {
		return &fault.Error{Code: "github.invalid_response", Message: "Missing ready PR"}
	}
	v := ci.Snapshot{SHA: run.ApprovedSHA}
	if run.CI != nil {
		v = *run.CI
	}
	v.CurrentHead = pr.Head.SHA
	if pr.State == github.Closed && !pr.Merged {
		return s.ciAttention(ctx, repo, run, v, "ci.pr_closed", "Pull request closed without merging; inspect preserved work")
	}
	if pr.Head.SHA != run.ApprovedSHA {
		v.Warning = "changed-after-approval: PR head changed after readiness; inspect it before merging"
	}
	return s.saveCI(ctx, run, v, "ci.updated")
}
