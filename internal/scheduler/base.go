package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// BaseGit keeps current-base observation and disposable verification separate
// from the implementer's working files and publication operations.
type BaseGit interface {
	CheckBase(context.Context, managedgit.Run, string, string) (managedgit.BaseMerge, error)
	VerificationCommit(context.Context, managedgit.Run, managedgit.BaseMerge) (string, error)
	VerificationRun(managedgit.Run, string, string) (managedgit.Run, error)
	EnsureVerification(context.Context, managedgit.Run, string, string) (managedgit.Run, error)
	InspectVerification(context.Context, managedgit.Run, string) error
	RemoveVerification(context.Context, managedgit.Run, string, string) error
}

func verificationCommands(repo config.Repository) []string {
	if repo.VerifyCommands != nil {
		return repo.VerifyCommands
	}
	return repo.Checks
}

// verifyBase observes a live journal before checking a new base. This prevents
// moving base refs or configuration edits from replacing a running command.
func (s *Scheduler) verifyBase(ctx context.Context, repo config.Repository, run workflow.Run, gitRun managedgit.Run, pr github.PullRequest, v *ci.Snapshot) (bool, error) {
	g, ok := s.deps.Git.(BaseGit)
	if !ok {
		return false, &fault.Error{Code: "git.base_check_unsupported", Message: "Git adapter cannot check the current base"}
	}
	b := v.Base
	if b != nil {
		copy := *b
		b = &copy
		v.Base = b
	}
	if b != nil && !b.Cleaned && b.ScratchPath != "" {
		if b.Status == "pending" {
			if _, err := os.Stat(b.ScratchPath); errors.Is(err, os.ErrNotExist) {
				steps, err := workflow.LoadSystemSteps(ctx, s.db, run.ID)
				if err != nil {
					return false, err
				}
				for _, step := range steps {
					if step.Step != "verify" || step.OwnerID != b.ID || step.Status != "running" {
						continue
					}
					status, err := s.deps.Runner.SessionStatus(ctx, runner.SessionRef{Name: step.ProcessSession, PhaseDir: step.PhaseDir})
					if err != nil {
						return false, err
					}
					if status.State == runner.SessionRunning {
						return false, &fault.Error{Code: "git.base_check", Message: "Verification scratch disappeared while its owned command is still running"}
					}
				}
			}
			scratch, err := g.EnsureVerification(ctx, gitRun, b.ID, b.CommitSHA)
			if err != nil {
				return false, err
			}
			// Owner and synthetic commit pin this invocation to one base/head pair.
			step, done, err := s.systemStep(ctx, repo, run, scratch, systemStepRequest{Step: "verify", OwnerID: b.ID, Commands: b.Commands, TargetSHA: b.CommitSHA})
			if err != nil || !done {
				return false, err
			}
			b.Status = "passed"
			if step.Status != "succeeded" {
				b.Status, b.Error = "failed", step.Error
			}
			b.Output = step.Output
			for _, result := range step.Results {
				if result.Status != "pass" {
					b.Error = fmt.Sprintf("%s: %s", result.Command, step.Error)
					b.Output = result.Output
					break
				}
			}
			// Save completion before removal, so restart never replays completed commands.
			if err := s.saveCI(ctx, run, *v, "ci.updated"); err != nil {
				return false, err
			}
		}
		if err := s.removeBaseScratch(ctx, run, gitRun, v); err != nil {
			return false, err
		}
	}
	merged, err := g.CheckBase(ctx, gitRun, pr.Base.Ref, run.ApprovedSHA)
	if err != nil {
		return false, err
	}
	// GitHub and the fetched ref must agree when an API base SHA is available.
	if pr.Base.SHA != "" && pr.Base.SHA != merged.BaseSHA {
		return false, &fault.Error{Code: "ci.base_changed", Message: "Fetched base differs from the GitHub observation; waiting for matching evidence"}
	}
	commands := verificationCommands(repo)
	same := b != nil && b.BaseBranch == merged.BaseBranch && b.BaseSHA == merged.BaseSHA && b.HeadSHA == merged.HeadSHA
	if same && b.Status == "failed" {
		return false, nil
	}
	if len(merged.Conflicts) > 0 {
		v.Base = &ci.BaseVerification{ID: uuid.NewString(), BaseBranch: merged.BaseBranch, BaseSHA: merged.BaseSHA, HeadSHA: merged.HeadSHA, TreeSHA: merged.TreeSHA, Conflicts: merged.Conflicts, Status: "conflict", Cleaned: true}
		return false, s.saveCI(ctx, run, *v, "ci.updated")
	}
	if same && b.Status == "passed" && slices.Equal(b.Commands, commands) {
		return true, nil
	}
	b = &ci.BaseVerification{ID: uuid.NewString(), BaseBranch: merged.BaseBranch, BaseSHA: merged.BaseSHA, HeadSHA: merged.HeadSHA, TreeSHA: merged.TreeSHA, Status: "passed", Commands: slices.Clone(commands), Cleaned: true}
	v.Base = b
	if len(commands) == 0 {
		return true, s.saveCI(ctx, run, *v, "ci.updated")
	}
	b.CommitSHA, err = g.VerificationCommit(ctx, gitRun, merged)
	if err != nil {
		return false, err
	}
	scratch, err := g.VerificationRun(gitRun, b.ID, b.CommitSHA)
	if err != nil {
		return false, err
	}
	b.ScratchPath, b.Cleaned, b.Status = scratch.Path, false, "pending"
	// Creation intent is durable before worktree add or command launch.
	if err := s.saveCI(ctx, run, *v, "ci.updated"); err != nil {
		return false, err
	}
	if _, err := g.EnsureVerification(ctx, gitRun, b.ID, b.CommitSHA); err != nil {
		return false, err
	}
	_, _, err = s.systemStep(ctx, repo, run, scratch, systemStepRequest{Step: "verify", OwnerID: b.ID, Commands: b.Commands, TargetSHA: b.CommitSHA})
	return false, err
}

func (s *Scheduler) removeBaseScratch(ctx context.Context, run workflow.Run, gitRun managedgit.Run, v *ci.Snapshot) error {
	b := v.Base
	if b == nil || b.Cleaned || b.ScratchPath == "" {
		return nil
	}
	g, ok := s.deps.Git.(BaseGit)
	if !ok {
		return &fault.Error{Code: "git.base_check_unsupported", Message: "Git adapter cannot clean verification scratch"}
	}
	// StopSession checks descendant ownership even after wrapper exit.
	steps, err := workflow.LoadSystemSteps(ctx, s.db, run.ID)
	if err != nil {
		return err
	}
	for _, step := range steps {
		if step.Step == "verify" && step.OwnerID == b.ID {
			if err := s.deps.Runner.StopSession(ctx, runner.SessionRef{Name: step.ProcessSession, PhaseDir: step.PhaseDir}); err != nil {
				return err
			}
		}
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE phase_attempts SET status='stopped' WHERE run_id=? AND step='verify' AND status='running'", run.ID); err != nil {
		return err
	}
	if err := g.RemoveVerification(ctx, gitRun, b.ID, b.CommitSHA); err != nil {
		return err
	}
	copy := *b
	copy.Cleaned = true
	v.Base = &copy
	return s.saveCI(ctx, run, *v, "ci.updated")
}

func baseRepairCause(v ci.Snapshot) string {
	if v.Base == nil || v.Base.HeadSHA != v.SHA {
		return ""
	}
	switch v.Base.Status {
	case "conflict":
		if len(v.Base.Conflicts) > 0 {
			return "git.base_conflict"
		}
	case "failed":
		if v.Base.Error != "" {
			return "git.base_verification_failed"
		}
	}
	return ""
}

func (s *Scheduler) repairBase(ctx context.Context, repo config.Repository, run workflow.Run, gitRun managedgit.Run, target github.PullRequest, v ci.Snapshot) error {
	v.RepairCause = baseRepairCause(v)
	if run.ReviewRound >= s.roundLimit(run) {
		message := "Current-base verification requires another independent review round; fix was not launched"
		if v.Base.Status == "conflict" {
			message += ": " + strings.Join(v.Base.Conflicts, ", ")
		} else {
			message += ": " + v.Base.Error + "\n" + v.Base.Output
		}
		return s.ciAttention(ctx, repo, run, v, v.RepairCause, message)
	}
	fresh, err := s.deps.GitHub.GetPullRequest(ctx, repo.Repo, run.PRNumber)
	if err != nil {
		return err
	}
	if fresh != nil && fresh.Merged {
		return s.merged(ctx, repo, run, fresh)
	}
	if !reviewHeadMatches(fresh, repo, run, gitRun, run.ApprovedSHA) {
		return s.ciAttention(ctx, repo, run, v, "ci.head_changed", "PR changed before current-base repair")
	}
	if !sameCITarget(target, *fresh) {
		return s.saveCI(ctx, run, v, "ci.updated")
	}
	if !s.deps.Now().UTC().Before(v.Deadline) {
		return s.ciAttention(ctx, repo, run, v, "ci.wait_timeout", "CI wait deadline expired during current-base repair reconciliation; no code fix was launched")
	}
	approved := ""
	_, err = s.workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.CIFailed, Metadata: workflow.MetadataPatch{CI: &v, ApprovedSHA: &approved}})
	return err
}

func pendingBaseScratch(run workflow.Run) bool {
	return run.CI != nil && run.CI.Base != nil && !run.CI.Base.Cleaned && run.CI.Base.ScratchPath != ""
}
