package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type HandbackGit interface {
	InspectHandback(context.Context, managedgit.Run, string) (string, error)
	CommitHandback(context.Context, managedgit.Run, string) (managedgit.CommitResult, error)
	PushHandback(context.Context, managedgit.Run, string, string) error
}

// Handback publishes under the shared lifecycle gate. Only a later tick starts
// the selected phase; publication intent survives a lost request or restart.
func (s *Scheduler) Handback(ctx context.Context, id string) (workflow.Run, error) {
	var result workflow.Run
	err := s.workflow.WithRunOperation(ctx, id, func(ctx context.Context, run workflow.Run) error {
		if run.State != workflow.Manual || run.TakeoverStatus != workflow.TakeoverManual {
			if len(run.Handbacks) > 0 && !run.Handbacks[len(run.Handbacks)-1].Pending && run.Handbacks[len(run.Handbacks)-1].Error == "" && !run.State.Terminal() {
				result = run
				return nil
			}
			return &fault.Error{Code: "handback.unavailable", Message: "Handback requires prepared manual control; request takeover first"}
		}
		repo, ok := s.repository(run.Repository)
		if !ok {
			return &fault.Error{Code: "config.repository_missing", Message: "Restore the run's repository configuration before handback"}
		}
		lock, err := runner.AcquireInteractive(s.workspace.Root, id)
		if err != nil {
			return err
		}
		defer lock.Close()
		if run.PendingHandback() == nil {
			v := workflow.HandbackSnapshot{Pending: true, RequestedAt: s.deps.Now().UTC().Format(time.RFC3339Nano)}
			data, err := json.Marshal(v)
			if err != nil {
				return err
			}
			if _, err = s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
				if _, err := tx.ExecContext(ctx, "UPDATE runs SET approved_sha=NULL WHERE id=?", id); err != nil {
					return events.Draft{}, err
				}
				_, err := tx.ExecContext(ctx, "INSERT INTO run_handbacks(run_id,pending,snapshot_json) VALUES(?,1,?)", id, string(data))
				return events.Draft{RunID: id, Type: "run.handback_requested", Payload: v}, err
			}); err != nil {
				return err
			}
			run, err = s.workflow.Get(ctx, id)
			if err != nil {
				return err
			}
		}
		err = s.resumeHandback(ctx, repo, run)
		var getErr error
		result, getErr = s.workflow.Get(ctx, id)
		return errors.Join(err, getErr)
	})
	return result, err
}

func (s *Scheduler) saveHandback(ctx context.Context, run workflow.Run, v workflow.HandbackSnapshot, kind string) error {
	_, err := s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		return events.Draft{RunID: run.ID, Type: kind, Payload: v}, v.Save(ctx, tx, run.ID)
	})
	return err
}

// resumeHandback's caller holds interactive ownership as well as the run gate.
func (s *Scheduler) resumeHandback(ctx context.Context, repo config.Repository, run workflow.Run) error {
	intent := run.PendingHandback()
	if intent == nil {
		return nil
	}
	v := *intent
	reject := func(code, message string) error {
		cause := &fault.Error{Code: code, Message: message}
		v.Pending = false
		v.Error = code + ": " + message
		if _, err := s.workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.OperationFailed, Failure: cause, Metadata: workflow.MetadataPatch{Handback: &v}}); err != nil {
			return err
		}
		return errors.Join(cause, s.attentionLabels(ctx, repo, run))
	}
	pending := func(cause error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		code := "handback.publication_failed"
		var coded *fault.Error
		if errors.As(cause, &coded) {
			code = coded.Code
		}
		message := code + ": Publication or reconciliation is incomplete; work is preserved. Retry handback after inspecting the application log."
		if v.Error != message {
			v.Error = message
			if err := s.saveHandback(ctx, run, v, "run.handback_pending"); err != nil {
				return err
			}
		}
		return &fault.Error{Code: "handback.publication_pending", Message: message, Err: cause}
	}
	publicationError := func(cause error) error {
		var coded *fault.Error
		if errors.As(cause, &coded) {
			switch coded.Code {
			case "handback.head_ambiguous", "handback.head_diverged", "git.worktree_mismatch", "git.origin_mismatch", "git.invalid_input", "review.head_changed":
				return reject("handback.git_ambiguous", "Git ownership or published ancestry changed during handback; inspect preserved manual work")
			}
		}
		return pending(cause)
	}
	var stopping bool
	if err := s.db.QueryRowContext(ctx, "SELECT stop_requested FROM runs WHERE id=?", run.ID).Scan(&stopping); err != nil {
		return err
	}
	if stopping || run.Merge != nil {
		return reject("handback.operation_pending", "Stop or merged-PR maintenance superseded handback")
	}
	// Check all recorded attempts, even those already marked stopped or failed.
	rows, err := s.db.QueryContext(ctx, "SELECT COALESCE(process_session,''),COALESCE(input_path,'') FROM phase_attempts WHERE run_id=?", run.ID)
	if err != nil {
		return err
	}
	var refs []runner.SessionRef
	for rows.Next() {
		var name, input string
		if err := rows.Scan(&name, &input); err != nil {
			rows.Close()
			return err
		}
		if name == "" || input == "" {
			rows.Close()
			return reject("handback.process_ambiguous", "An owned attempt lacks process identity; inspect it before handback")
		}
		refs = append(refs, runner.SessionRef{Name: name, PhaseDir: filepath.Dir(input)})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, ref := range refs {
		status, err := s.deps.Runner.SessionStatus(ctx, ref)
		if err != nil {
			return pending(err)
		}
		if status.State != runner.SessionExited {
			return reject("handback.process_ambiguous", "An owned phase process is running or its exit cannot be verified; stop it before handback")
		}
	}
	if run.Review != nil && !run.Review.Restored {
		return reject("handback.review_unrestored", "Review restoration is unfinished; inspect preserved manual work")
	}
	_, gitRun, err := s.context(ctx, run)
	if err != nil {
		return reject("handback.context_missing", "Inspect the saved worktree and branch before handback")
	}
	g, ok := s.deps.Git.(HandbackGit)
	if !ok {
		return reject("handback.git_unsupported", "Git adapter cannot safely publish manual work")
	}
	number := run.PRNumber
	if v.Inspected {
		number = v.PRNumber
	}
	var pr *github.PullRequest
	if number > 0 {
		pr, err = s.deps.GitHub.GetPullRequest(ctx, repo.Repo, number)
	} else {
		pr, err = s.deps.GitHub.FindPullRequest(ctx, repo.Repo, gitRun.Branch)
	}
	if err != nil {
		var coded *fault.Error
		if errors.As(err, &coded) && coded.Code == "pr.multiple_matches" {
			return reject("handback.pr_ambiguous", "Multiple PRs match the run branch across states; inspect GitHub before publishing manual work")
		}
		return pending(err)
	}
	if number > 0 && pr == nil {
		return reject("handback.pr_ambiguous", "Saved PR could not be observed; inspect GitHub")
	}
	if pr != nil {
		if pr.Merged || pr.State != github.Open {
			return reject("handback.pr_closed", "PR is closed or merged; inspect preserved work before continuing")
		}
		if pr.Head.SHA == "" || !reviewHeadMatches(pr, repo, workflow.Run{RunMetadata: workflow.RunMetadata{PRNumber: pr.Number}}, gitRun, pr.Head.SHA) {
			return reject("handback.pr_ambiguous", "PR ownership differs from the run branch or repository")
		}
	}
	issue, err := s.deps.GitHub.GetIssue(ctx, repo.Repo, run.IssueNumber)
	if err != nil {
		return pending(err)
	}
	if issue.State != github.Open {
		return reject("github.issue_closed", "Issue is closed; inspect preserved work before handback")
	}
	if !v.Inspected {
		target := ""
		if pr != nil {
			target = pr.Head.SHA
			v.PRNumber = pr.Number
		}
		previous, err := g.InspectHandback(ctx, gitRun, target)
		if err != nil {
			return reject("handback.git_ambiguous", "Worktree, branch or published ancestry differs; preserve and inspect manual work")
		}
		v.PreviousSHA = previous
		v.Inspected = true
		v.NextState, v.NextPhase = workflow.Active, workflow.Implement
		v.Round = run.ReviewRound
		if pr != nil {
			v.NextPhase = workflow.Review
			if run.Review != nil && run.Review.Round == run.ReviewRound {
				if run.Review.Accepted {
					v.Round++
				} else {
					v.AttemptFrom = run.Review.Attempt + 1
				}
			}
			if v.Round < 1 {
				v.Round = 1
			}
			if v.Round > s.roundLimit(run) {
				v.GrantedRound = v.Round
			}
		} else if a, err := s.lastAttempt(ctx, run.ID); err == nil {
			v.AttemptFrom = a.number + 1
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		v.Error = ""
		if err := s.saveHandback(ctx, run, v, "run.handback_inspected"); err != nil {
			return err
		}
	} else if (pr == nil) != (v.PRNumber == 0) || (pr != nil && pr.Head.SHA != v.PreviousSHA && pr.Head.SHA != v.CommitSHA) {
		return reject("handback.pr_ambiguous", "PR changed during handback; inspect preserved work")
	}
	if v.CommitSHA == "" {
		result, err := g.CommitHandback(ctx, gitRun, v.PreviousSHA)
		if err != nil {
			return publicationError(err)
		}
		v.CommitSHA = result.SHA
		v.Error = ""
		if err := s.saveHandback(ctx, run, v, "run.handback_committed"); err != nil {
			return err
		}
	}
	if err := g.PushHandback(ctx, gitRun, v.PreviousSHA, v.CommitSHA); err != nil {
		return publicationError(err)
	}
	if v.PRNumber > 0 {
		fresh, err := s.deps.GitHub.GetPullRequest(ctx, repo.Repo, v.PRNumber)
		if err != nil {
			return pending(err)
		}
		if fresh == nil || fresh.Merged || fresh.State != github.Open {
			return reject("handback.pr_closed", "PR closed or disappeared during publication; inspect preserved work")
		}
		if !reviewHeadMatches(fresh, repo, workflow.Run{RunMetadata: workflow.RunMetadata{PRNumber: v.PRNumber}}, gitRun, v.CommitSHA) {
			return pending(&fault.Error{Code: "handback.head_ambiguous", Message: "Published PR head does not match manual work"})
		}
	}
	v.Pending = false
	v.Error = ""
	empty := ""
	patch := workflow.MetadataPatch{Handback: &v, ReviewRound: &v.Round, ApprovedSHA: &empty}
	if v.PRNumber > 0 {
		patch.PRNumber = &v.PRNumber
	}
	selected, err := s.workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.HandBack, NextPhase: v.NextPhase, Metadata: patch})
	if err != nil {
		return err
	}
	return s.retryLabels(ctx, repo, selected)
}
