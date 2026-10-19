package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// Finding identifies an artifact requiring human inspection. Reconciliation
// never resets orphaned claims or deletes orphaned worktrees or sessions.
type Finding struct {
	Code        string `json:"code"`
	Repository  string `json:"repository,omitempty"`
	IssueNumber int    `json:"issue_number,omitempty"`
	RunID       string `json:"run_id,omitempty"`
	Path        string `json:"path,omitempty"`
	Session     string `json:"session,omitempty"`
	Message     string `json:"message"`
}

type ReconcileReport struct {
	Findings []Finding `json:"findings"`
}

// scopedFailure is a GitHub failure that belongs to one repository or run.
// Reconcile reports it and leaves it for later ticks, but it must not gate
// dispatch or startup for unrelated work.
type scopedFailure struct {
	finding Finding
	err     error
}

func (f *scopedFailure) Error() string { return f.finding.String() }
func (f *scopedFailure) Unwrap() error { return f.err }

// classifyGitHub reports whether err is made entirely of failures from the
// GitHub boundary. A conclusive answer such as not_found or forbidden passes
// only after a caller scoped it to the repository or run that produced it.
func classifyGitHub(err error, conclusive bool) bool {
	switch failure := err.(type) {
	case *scopedFailure:
		return classifyGitHub(failure.err, true)
	case *fault.Error:
		if strings.HasPrefix(failure.Code, "github.") {
			return conclusive || githubRetryable[failure.Code]
		}
		if failure.Err == nil {
			return false
		}
		return classifyGitHub(failure.Err, conclusive)
	case interface{ Unwrap() []error }:
		for _, joined := range failure.Unwrap() {
			if !classifyGitHub(joined, conclusive) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return classifyGitHub(failure.Unwrap(), conclusive)
	default:
		return false
	}
}

// githubFailure reports whether err is made entirely of GitHub boundary
// failures, including conclusive answers.
func githubFailure(err error) bool { return classifyGitHub(err, true) }

// scopeGitHubFailure wraps err as a finding when it is a GitHub failure. Local
// state stays unwrapped so it can still stop dispatch.
func scopeGitHubFailure(code, repository string, issue int, runID string, err error) *scopedFailure {
	if !githubFailure(err) {
		return nil
	}
	finding := Finding{Code: code, Repository: repository, IssueNumber: issue, RunID: runID, Message: err.Error()}
	return &scopedFailure{finding: finding, err: err}
}

// reconcileResult separates failures that may be scoped to one repository or
// run from local-state failures that make dispatch unsafe everywhere.
type reconcileResult struct {
	report              ReconcileReport
	activeFailures      []Finding
	scopedErrors        []error
	fatalErrors         []error
	observed            bool
	blockedRepositories map[string]bool
	blockedIssues       map[string]bool
}

func (r *reconcileResult) addScoped(failure *scopedFailure) {
	r.activeFailures = append(r.activeFailures, failure.finding)
	r.report.Findings = append(r.report.Findings, failure.finding)
	r.scopedErrors = append(r.scopedErrors, failure)
}

func (r *reconcileResult) blockRepository(repository string) {
	if r.blockedRepositories == nil {
		r.blockedRepositories = map[string]bool{}
	}
	r.blockedRepositories[strings.ToLower(repository)] = true
}

func (r *reconcileResult) blockIssue(repository string, number int) {
	if r.blockedIssues == nil {
		r.blockedIssues = map[string]bool{}
	}
	r.blockedIssues[issueKey(repository, number)] = true
}

func (r *reconcileResult) err() error {
	failures := append([]error{}, r.fatalErrors...)
	return errors.Join(append(failures, r.scopedErrors...)...)
}

// githubRetryable names the unscoped GitHub adapter failures that a later tick
// retries: the service could not be reached, was rate limited, was cancelled, or
// did not answer cleanly. Conclusive unscoped answers stay absent because their
// caller has not established which repository or run they belong to.
var githubRetryable = map[string]bool{
	"github.unavailable":      true,
	"github.rate_limited":     true,
	"github.canceled":         true,
	"github.timeout":          true,
	"github.command_failed":   true,
	"github.invalid_response": true,
}

// GitHubFailure reports whether err can be left for later ticks. A scoped
// GitHub failure is always deferrable, including a conclusive not_found or
// forbidden answer, because it no longer gates unrelated work. An unscoped
// error must come from a retryable GitHub failure and nothing else. A coded
// wrapper classifies by its cause, so pending handback state wrapped around a
// GitHub failure stays deferrable. Every error joined into the tree must
// classify as deferrable; anything else (database, workspace, Git, harness,
// runner, or an unscoped conclusive GitHub answer) makes dispatch unsafe and
// returns false.
func GitHubFailure(err error) bool { return classifyGitHub(err, false) }

// Reconcile observes and advances existing runs without claiming any new work.
// Run's first tick and every later Tick use this pass before dispatch; local
// failures still gate, while scoped GitHub failures do not.
func (s *Scheduler) Reconcile(ctx context.Context) (ReconcileReport, error) {
	if err := s.tick.Lock(ctx); err != nil {
		return ReconcileReport{}, err
	}
	defer s.tick.Unlock()
	s.beginPoll()
	result := s.reconcile(ctx)
	if result.observed {
		s.setReconcileFailures(result.activeFailures)
	}
	return result.report, result.err()
}

func (s *Scheduler) reconcile(ctx context.Context) reconcileResult {
	result := reconcileResult{report: ReconcileReport{Findings: []Finding{}}}
	if err := s.workflow.ReconcileAgentPorts(ctx, s.cfg.AgentPorts); err != nil {
		result.fatalErrors = append(result.fatalErrors, err)
		return result
	}
	if err := s.reconcileHarnessChecks(ctx); err != nil {
		result.fatalErrors = append(result.fatalErrors, err)
		return result
	}
	if err := s.workflow.ReconcileHarnessLimits(ctx, s.deps.Now().UTC()); err != nil {
		result.fatalErrors = append(result.fatalErrors, err)
		return result
	}
	runs, err := s.tickRuns(ctx)
	if err != nil {
		result.fatalErrors = append(result.fatalErrors, err)
		return result
	}
	reconcilable := map[string]bool{}
	for _, run := range runs {
		if run.PendingRetry() == nil && run.State.Terminal() && !pendingBaseScratch(run) && !(run.State == workflow.Completed && run.Merge != nil && run.Merge.Pending()) {
			continue
		}
		repo, ok := s.runRepository(run)
		if !ok && run.Merge != nil {
			repo = config.Repository{Repo: run.Repository}
			ok = true
		}
		if !ok {
			result.report.Findings = append(result.report.Findings, Finding{Code: "reconcile.repository_missing", Repository: run.Repository, IssueNumber: run.IssueNumber, RunID: run.ID, Message: "Run repository is absent from configuration"})
			continue
		}
		err := s.workflow.WithRunOperation(ctx, run.ID, func(opCtx context.Context, current workflow.Run) error {
			return s.reconcileRun(opCtx, repo, current)
		})
		if err != nil {
			if failure := scopeGitHubFailure("reconcile.run_failed", repo.Repo, run.IssueNumber, run.ID, err); failure != nil {
				result.addScoped(failure)
				result.blockIssue(repo.Repo, run.IssueNumber)
			} else {
				result.fatalErrors = append(result.fatalErrors, err)
			}
		}
		// A claim interrupted before saving issue context can be reconstructed
		// from the known run identity. Other runs need their durable context.
		_, _, err = s.context(ctx, run)
		if err == nil {
			reconcilable[issueKey(run.Repository, run.IssueNumber)] = true
		}
	}
	if err := s.publishAttention(ctx); err != nil {
		result.fatalErrors = append(result.fatalErrors, err)
	}
	if err := s.publishVerification(ctx); err != nil {
		result.fatalErrors = append(result.fatalErrors, err)
	}
	if err := s.publishReports(ctx); err != nil {
		result.fatalErrors = append(result.fatalErrors, err)
	}
	// Scan all configured repositories, even when paused, disabled, or full.
	for _, repo := range s.cfg.Repositories {
		issues, err := s.listIssues(ctx, repo)
		if err != nil {
			if failure := scopeGitHubFailure("reconcile.repository_failed", repo.Repo, 0, "", err); failure != nil {
				result.addScoped(failure)
				result.blockRepository(repo.Repo)
			} else {
				result.fatalErrors = append(result.fatalErrors, err)
			}
			continue
		}
		for _, issue := range issues {
			if issue.State == github.Open && hasLabel(issue, repo.Labels.Running) && !reconcilable[issueKey(repo.Repo, issue.Number)] {
				result.report.Findings = append(result.report.Findings, Finding{Code: "reconcile.orphaned_claim", Repository: repo.Repo, IssueNumber: issue.Number, Message: "Running label has no reconcilable local run; manual action required"})
			}
		}
	}
	if err := s.orphanedArtifacts(ctx, &result.report); err != nil {
		result.fatalErrors = append(result.fatalErrors, err)
	}
	observed := map[string]bool{}
	for _, finding := range result.report.Findings {
		data, err := json.Marshal(finding)
		if err != nil {
			result.fatalErrors = append(result.fatalErrors, err)
			continue
		}
		key := string(data)
		if s.reportedFindings[key] {
			observed[key] = true
			continue
		}
		if _, err := s.bus.Publish(ctx, events.Draft{Type: finding.Code, Payload: finding}); err != nil {
			result.fatalErrors = append(result.fatalErrors, err)
		} else {
			observed[key] = true
		}
	}
	s.reportedFindings = observed
	result.observed = true
	return result
}

func (s *Scheduler) reconcileRun(ctx context.Context, repo config.Repository, run workflow.Run) error {
	if run.State != workflow.WaitingForCI && pendingBaseScratch(run) {
		_, gitRun, err := s.context(ctx, run)
		if err != nil {
			return err
		}
		v := *run.CI
		if err := s.removeBaseScratch(ctx, run, gitRun, &v); err != nil {
			return err
		}
		run.CI = &v
	}
	if run.PendingHandback() != nil {
		var stopping bool
		if err := s.db.QueryRowContext(ctx, "SELECT stop_requested FROM runs WHERE id=?", run.ID).Scan(&stopping); err != nil {
			return err
		}
		if stopping {
			return s.stopRun(ctx, run)
		}
		lock, err := runner.AcquireInteractive(s.workspace.Root, run.ID)
		if err != nil {
			return err
		}
		defer lock.Close()
		return s.resumeHandback(ctx, repo, run)
	}
	if run.TakeoverStatus == workflow.TakeoverRequested {
		return s.prepareTakeover(ctx, run)
	}
	if run.PendingRetry() != nil {
		return s.resumeRetry(ctx, repo, run)
	}
	if run.State.Terminal() {
		if run.State == workflow.Completed && run.Merge != nil {
			return s.cleanupMerge(ctx, run)
		}
		return nil
	}
	if run.Merge != nil {
		return s.finishMerge(ctx, run)
	}
	var stopRequested bool
	if err := s.db.QueryRowContext(ctx, "SELECT stop_requested FROM runs WHERE id=?", run.ID).Scan(&stopRequested); err != nil {
		return err
	}
	// Failed merge reads must not bypass durable stop recovery or CI deadlines.
	if run.PRNumber > 0 {
		pr, err := s.deps.GitHub.GetPullRequest(ctx, repo.Repo, run.PRNumber)
		if err != nil && !stopRequested && run.State != workflow.WaitingForCI {
			return err
		}
		if err == nil && pr != nil && pr.Merged {
			return s.merged(ctx, repo, run, pr)
		}
	}
	// Honor durable stop intent before restoring labels or inspecting work.
	if stopRequested {
		return s.stopRun(ctx, run)
	}
	if run.State == workflow.WaitingForHarness {
		if err := s.resumeHarnessWait(ctx, run); err != nil {
			return err
		}
		return s.retryProgressLabels(ctx, repo, run)
	}
	if run.State == workflow.WaitingForCI {
		if err := s.waitCI(ctx, repo, run); err != nil {
			return err
		}
		return s.retryProgressLabels(ctx, repo, run)
	}
	if run.State == workflow.ReadyToMerge {
		if err := s.observeReady(ctx, repo, run); err != nil {
			return err
		}
		return s.retryProgressLabels(ctx, repo, run)
	}
	issue, err := s.deps.GitHub.GetIssue(ctx, repo.Repo, run.IssueNumber)
	if err != nil {
		return err
	} // An unavailable external service never revokes a run.
	_, gitRun, contextErr := s.context(ctx, run)
	if errors.Is(contextErr, sql.ErrNoRows) && run.State == workflow.Claiming {
		contextErr = s.recoverIssueSnapshot(ctx, repo, run.ID, issue)
	}
	// Check PR even when the issue has closed; it may have closed through merge.
	var pr *github.PullRequest
	if run.PRNumber > 0 {
		pr, err = s.deps.GitHub.GetPullRequest(ctx, repo.Repo, run.PRNumber)
	} else if gitRun.ID != "" {
		pr, err = s.deps.GitHub.FindOpenPullRequest(ctx, repo.Repo, gitRun.Branch)
	}
	if err != nil {
		return err
	}
	if pr != nil && pr.Merged {
		return s.merged(ctx, repo, run, pr)
	}
	var inconsistency error
	if contextErr != nil {
		// Without recoverable context the claim itself is orphaned. Keep its
		// running label and artifacts; a subsequent tick must not reset it.
		if run.State == workflow.Manual || run.State == workflow.NeedsAttention {
			return nil
		}
		_, err := s.workflow.Transition(ctx, run.ID, workflow.Request{Trigger: workflow.OperationFailed, Failure: &fault.Error{Code: "reconcile.context_missing", Message: "Run context cannot be reconciled; inspect persisted artifacts", Err: contextErr}})
		return err
	}
	// A persisted review snapshot owns Git validation until restoration. Checking
	// branch attachment while its process is editing Git can observe a transient
	// detached HEAD, stop the reviewer there, and make safe restoration impossible.
	reviewOwnsGit := run.State == workflow.Active && run.Phase == workflow.Review && run.Review != nil && run.Review.Status == "running" && !run.Review.Restored
	if gitRun.ID != "" {
		if !reviewOwnsGit {
			if err := s.deps.Git.Inspect(ctx, gitRun); err != nil {
				inconsistency = err
			}
		}
	} else if run.State != workflow.Claiming && run.State != workflow.Preparing && run.State != workflow.NeedsAttention {
		inconsistency = &fault.Error{Code: "git.ownership_missing", Message: "Run has no persisted Git ownership"}
	}
	// Observe every persisted running attempt, including states whose future
	// phase execution is not yet implemented by M1.
	refs, err := s.runSessions(ctx, run.ID)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if systemSession(run, ref.Name) {
			continue
		} // System steps own exit recovery and safe replay.
		if run.Phase == workflow.Review {
			continue
		} // Review owns process recovery and restoration.
		status, err := s.deps.Runner.SessionStatus(ctx, ref)
		if err != nil {
			var coded *fault.Error
			if errors.As(err, &coded) && (coded.Code == "phase.exit_invalid" || coded.Code == "phase.exit_read_failed") {
				inconsistency = err
				continue
			}
			return err
		}
		if status.State == runner.SessionMissing {
			inconsistency = &fault.Error{Code: "phase.session_missing", Message: "Session has no exit metadata; inspect before retrying"}
		}
	}
	// Human-controlled states are observed but never automatically resumed.
	if run.State == workflow.Manual {
		return nil
	}
	if run.State == workflow.NeedsAttention {
		if err := s.restorePendingReview(ctx, run); err != nil {
			return err
		}
		return s.attentionLabels(ctx, repo, run)
	}
	if pr != nil && pr.State != github.Open {
		inconsistency = &fault.Error{Code: "reconcile.pr_closed", Message: "Pull request closed while offline; inspect before continuing"}
	}
	if issue.State != github.Open {
		inconsistency = &fault.Error{Code: "github.issue_closed", Message: "Issue is no longer open; inspect before continuing"}
	}
	if inconsistency != nil {
		if run.State == workflow.Active && run.Phase == workflow.Review {
			a, err := s.lastReview(ctx, run)
			if err == nil && a.Status == "running" {
				return s.abortReview(ctx, repo, run, gitRun, &a, inconsistency)
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		return s.recordAttention(ctx, repo, run, inconsistency)
	}
	if run.State != workflow.Claiming {
		if !hasLabel(issue, repo.Labels.Running) {
			if err := s.deps.GitHub.AddLabel(ctx, repo.Repo, run.IssueNumber, repo.Labels.Running); err != nil {
				return err
			}
		}
		if hasLabel(issue, repo.Labels.Ready) {
			if err := s.deps.GitHub.RemoveLabel(ctx, repo.Repo, run.IssueNumber, repo.Labels.Ready); err != nil {
				return err
			}
		}
	}
	if len(run.Retries) > 0 || len(run.Handbacks) > 0 {
		if err := s.retryLabels(ctx, repo, run); err != nil {
			return err
		}
	}
	return s.advanceRun(ctx, repo, run)
}

func (s *Scheduler) runSessions(ctx context.Context, id string) ([]runner.SessionRef, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT process_session,input_path FROM phase_attempts WHERE run_id=? AND status='running' AND process_session IS NOT NULL", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []runner.SessionRef
	for rows.Next() {
		var name string
		var input sql.NullString
		if err := rows.Scan(&name, &input); err != nil {
			return nil, err
		}
		refs = append(refs, runner.SessionRef{Name: name, PhaseDir: filepath.Dir(input.String)})
	}
	return refs, rows.Err()
}

func (s *Scheduler) orphanedArtifacts(ctx context.Context, report *ReconcileReport) error {
	// Terminal and human-controlled runs may intentionally retain their artifacts.
	knownPaths := map[string]bool{}
	knownSessions := map[string]bool{}
	physicalRoot, err := filepath.EvalSymlinks(s.workspace.Root)
	if err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT worktree_path FROM runs WHERE worktree_path IS NOT NULL")
	if err != nil {
		return err
	}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			rows.Close()
			return err
		}
		if physical, err := filepath.EvalSymlinks(path); err == nil {
			path = physical
		}
		knownPaths[path] = true
		// Registered Git metadata may retain a physical path after a removed
		// directory can no longer be resolved through the workspace's alias.
		if relative, err := filepath.Rel(s.workspace.Root, path); err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			knownPaths[filepath.Join(physicalRoot, relative)] = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	rows, err = s.db.QueryContext(ctx, "SELECT process_session FROM phase_attempts WHERE process_session IS NOT NULL UNION SELECT process_session FROM harness_checks")
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		knownSessions[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	runs, err := s.tickRuns(ctx)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if pendingBaseScratch(run) {
			path := run.CI.Base.ScratchPath
			knownPaths[path] = true
			if relative, err := filepath.Rel(s.workspace.Root, path); err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				knownPaths[filepath.Join(physicalRoot, relative)] = true
			}
		}
	}
	paths, pathErr := s.deps.Git.ListWorktrees(ctx)
	for _, path := range paths {
		if !knownPaths[path] {
			report.Findings = append(report.Findings, Finding{Code: "reconcile.orphaned_worktree", Path: path, Message: "Worktree has no persisted run; preserved for inspection"})
		}
	}
	names, sessionErr := s.deps.Runner.ListSessions(ctx)
	for _, name := range names {
		if !knownSessions[name] {
			report.Findings = append(report.Findings, Finding{Code: "reconcile.orphaned_session", Session: name, Message: "Tmux session has no persisted attempt; preserved for inspection"})
		}
	}
	return errors.Join(pathErr, sessionErr)
}

// Key identifies one finding's subject so discovery and reconciliation cannot
// show duplicate dashboard rows for it. The message is not part of the identity
// because a later read may refine the same failure's diagnostic text.
func (f Finding) Key() string {
	return fmt.Sprintf("%s|%s|%d|%s|%s|%s", f.Code, strings.ToLower(f.Repository), f.IssueNumber, f.RunID, f.Path, f.Session)
}

func (f Finding) String() string {
	identity := f.Path
	if f.Repository != "" {
		identity = f.Repository
		if f.IssueNumber > 0 {
			identity = fmt.Sprintf("%s#%d", f.Repository, f.IssueNumber)
		}
	}
	if f.Session != "" {
		identity = f.Session
	}
	return fmt.Sprintf("%s: %s: %s", f.Code, identity, f.Message)
}
