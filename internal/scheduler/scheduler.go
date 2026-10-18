// Package scheduler dispatches eligible issues and advances the bounded implementation, review, and fix loop.
package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/workflow"
	"github.com/rcpassos/mergeyard/internal/workspace"
)

// GitHub is the external issue, label, and PR boundary.
type GitHub interface {
	VerifyIssueTrust(context.Context, string, github.Issue, string, []string) (github.TrustDecision, error)
	RecheckIssueTrust(context.Context, string, github.Issue, string, []string) (github.TrustDecision, error)
	EnsureTrustRefusalComment(context.Context, string, int, string) error
	EnsureHarnessOverrideComment(context.Context, string, int, string) error
	ListLabeledIssues(context.Context, string, []string) ([]github.Issue, error)
	GetIssue(context.Context, string, int) (github.Issue, error)
	ListIssueComments(context.Context, string, int, []string) (github.IssueComments, error)
	GetPullRequest(context.Context, string, int) (*github.PullRequest, error)
	UnresolvedBlockers(context.Context, string, int) ([]github.Issue, error)
	AddLabel(context.Context, string, int, string) error
	RemoveLabel(context.Context, string, int, string) error
	FindOpenPullRequest(context.Context, string, string) (*github.PullRequest, error)
	FindPullRequest(context.Context, string, string) (*github.PullRequest, error)
	CreateDraftPullRequest(context.Context, string, string, string, github.PullRequestContent) (*github.PullRequest, error)
	UpdatePullRequest(context.Context, string, int, github.PullRequestContent) (*github.PullRequest, error)
}

// Git is the managed Git boundary; implementations must preserve run ownership.
type Git interface {
	Prepare(context.Context, managedgit.PrepareRequest) (managedgit.Run, error)
	CommitAndPush(context.Context, managedgit.Run, managedgit.Phase) (managedgit.CommitResult, error)
	Inspect(context.Context, managedgit.Run) error
	ListWorktrees(context.Context) ([]string, error)
}

// Resources are the shared components of an exclusively owned runtime.
// Workflow must be the same instance used for lifecycle controls. Pass the
// runtime's Control to share the dashboard claim gate; nil creates a local gate.
type Resources struct {
	DB        *sql.DB
	Events    *events.Bus
	Workflow  *workflow.Workflow
	Workspace *workspace.Workspace
	Control   *Control
}

type Dependencies struct {
	// Harnesses optionally supplies adapters at the external harness boundary.
	Harnesses map[string]harness.HarnessAdapter
	GitHub    GitHub
	Git       Git
	Runner    runner.Runner
	// Now controls CI observation windows; nil uses wall time.
	Now func() time.Time
	// Env is the complete harness environment; callers explicitly select it.
	Env map[string]string
}

type Scheduler struct {
	cfg              config.Config
	db               *sql.DB
	bus              *events.Bus
	workflow         *workflow.Workflow
	workspace        *workspace.Workspace
	deps             Dependencies
	harnesses        map[string]harness.HarnessAdapter
	tick             sync.Mutex
	control          *Control
	running          atomic.Bool
	listingFetch     sync.Mutex               // serializes listing fetches so concurrent readers share one
	listings         listingCache             // this poll's label-filtered issue snapshot per repository
	blockers         keyedCache[blockerEntry] // dashboard blocker reads keyed by unchanged issue snapshots
	reportedFindings map[string]bool          // protected by tick; unchanged findings emit once per scheduler lifetime
}

// New requires an exclusively owned runtime workspace.
func New(cfg config.Config, resources Resources, deps Dependencies) (*Scheduler, error) {
	if resources.DB == nil || resources.Events == nil || resources.Workflow == nil || resources.Workspace == nil {
		return nil, &fault.Error{Code: "internal.scheduler_runtime", Message: "Scheduler requires a database, event bus, shared workflow, and workspace"}
	}
	control := resources.Control
	if control == nil {
		control = NewControl(resources.Events)
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 30 * time.Second
	}
	if cfg.Concurrency < 1 {
		return nil, &fault.Error{Code: "config.invalid_concurrency", Message: "Scheduler concurrency must be positive"}
	}
	if cfg.AgentPorts == (config.AgentPorts{}) {
		cfg.AgentPorts = config.AgentPorts{Base: 5100, Size: 10}
	}
	if err := cfg.AgentPorts.Validate(); err != nil {
		return nil, err
	}
	if cfg.CITimeout == 0 {
		cfg.CITimeout = time.Hour
	}
	if cfg.CITimeout < 0 {
		return nil, &fault.Error{Code: "config.invalid_duration", Message: "CI timeout must be positive"}
	}
	if cfg.UsageLimits.Cooldown <= 0 {
		cfg.UsageLimits.Cooldown = 30 * time.Minute
	}
	if cfg.UsageLimits.MaxWaits < 1 {
		cfg.UsageLimits.MaxWaits = 3
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	adapters := map[string]harness.HarnessAdapter{"claude": harness.NewClaude(cfg.Agents.Claude), "codex": harness.NewCodex(cfg.Agents.Codex)}
	for name, adapter := range deps.Harnesses {
		if adapter == nil || adapter.Type() != name {
			return nil, &fault.Error{Code: "config.invalid_agent", Message: "Harness override must match its type"}
		}
		adapters[name] = adapter
	}
	for _, repo := range cfg.Repositories {
		if repo.Enabled {
			for _, role := range []config.Role{repo.Implementer, repo.Reviewer} {
				if err := validateHarnessRole(adapters, role); err != nil {
					return nil, err
				}
			}
		}
	}
	if deps.GitHub == nil {
		deps.GitHub = github.New(nil)
	}
	if deps.Git == nil {
		deps.Git = managedgit.New(resources.Workspace)
	}
	if deps.Runner == nil {
		deps.Runner = runner.NewLocal(runner.Options{})
	}
	env := make(map[string]string, len(deps.Env))
	for k, v := range deps.Env {
		env[k] = v
	}
	deps.Env = env
	return &Scheduler{cfg: cfg, db: resources.DB, bus: resources.Events, workflow: resources.Workflow, workspace: resources.Workspace, deps: deps, harnesses: adapters, control: control}, nil
}

// Pause only stops new claims. Existing agent attempts continue on ticks.
func (s *Scheduler) Pause(ctx context.Context, paused bool) error {
	if paused {
		return s.control.Pause(ctx)
	}
	return s.control.Resume(ctx)
}

// Run polls immediately and then at the configured interval. A transient tick
// failure is logged and retried next tick; cancellation never stops tmux agents.
func (s *Scheduler) Run(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return &fault.Error{Code: "internal.scheduler_running", Message: "Scheduler is already running"}
	}
	defer s.running.Store(false)
	timer := time.NewTicker(s.cfg.PollInterval)
	defer timer.Stop()
	identityTimer := time.NewTicker(250 * time.Millisecond)
	defer identityTimer.Stop()
	poll := func() {
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "scheduler tick", "error", err)
		}
	}
	poll()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			poll()
		case <-identityTimer.C:
			if err := s.observeIdentities(ctx); err != nil && ctx.Err() == nil {
				slog.ErrorContext(ctx, "harness identity observation", "error", err)
			}
		}
	}
}

// Runs returns the persisted lifecycle snapshots, including durable review verdicts.
func (s *Scheduler) Runs(ctx context.Context) ([]workflow.Run, error) {
	runs, err := s.storedRuns(ctx)
	if err != nil {
		return nil, err
	}
	for i, run := range runs {
		run.RetryEligible, err = s.RetryEligible(ctx, run)
		if err != nil {
			return nil, err
		}
		runs[i] = s.runSnapshot(run)
	}
	return runs, nil
}

// Tick serializes reconciliation and dispatch. It observes running processes
// once and returns, so long phases do not block new claims or pause/resume.
func (s *Scheduler) Tick(ctx context.Context) error {
	s.tick.Lock()
	defer s.tick.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	s.beginPoll()
	_, err := s.reconcile(ctx)
	if err != nil {
		return err
	}
	var failures []error
	if err := s.reconcileHarnessOverrideRefusals(ctx); err != nil {
		failures = append(failures, err)
	}
	if err := s.reconcileRefusals(ctx); err != nil {
		failures = append(failures, err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if s.control.Paused() {
		return errors.Join(failures...)
	}
	runs, err := s.tickRuns(ctx)
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	capacity := capacityFor(runs, false)
	for _, repo := range s.cfg.Repositories {
		if !repo.Enabled || capacity.slots >= s.cfg.Concurrency || s.control.Paused() {
			continue
		}
		issues, err := s.listIssues(ctx, repo)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		for _, issue := range orderedIssues(issues) {
			capacity.paused = s.control.Paused()
			candidate, err := s.inspectCandidate(ctx, repo, issue, capacity)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if candidate.trust.Allowed || candidate.reason == "untrusted_labeler" {
				if _, err := s.applyTrustDecision(ctx, repo, candidate.issue, candidate.trust, candidate.trustErr); err != nil {
					failures = append(failures, err)
					continue
				}
				if candidate.reason == "untrusted_labeler" {
					continue
				}
			}
			if candidate.settingsErr != nil {
				if err := s.refuseHarnessOverride(ctx, repo, issue.Number, candidate.settingsErr.Error()); err != nil {
					failures = append(failures, err)
				}
				continue
			}
			if candidate.decision != "would_claim" {
				continue
			}
			issue = candidate.issue
			settings := candidate.settings
			s.control.mu.Lock()
			if s.control.paused {
				s.control.mu.Unlock()
				break
			}
			run, err := s.workflow.Transition(ctx, uuid.NewString(), workflow.Request{Trigger: workflow.IssueClaimed, Repository: repo.Repo, IssueNumber: issue.Number, RoleSettings: settings, AgentPorts: &s.cfg.AgentPorts})
			s.control.mu.Unlock()
			if err != nil {
				failures = append(failures, err)
				continue
			}
			capacity.reserve(repo.Repo, issue.Number)
			if err := s.saveIssue(ctx, run.ID, issue); err != nil {
				failures = append(failures, s.attention(ctx, repo, run, err))
				continue
			}
			if err := s.advance(ctx, repo, run); err != nil {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}

func issueKey(repo string, n int) string { return strings.ToLower(repo) + "/" + strconv.Itoa(n) }
func hasLabel(issue github.Issue, name string) bool {
	for _, label := range issue.Labels {
		if strings.EqualFold(label.Name, name) {
			return true
		}
	}
	return false
}
func (s *Scheduler) repository(name string) (config.Repository, bool) {
	for _, repo := range s.cfg.Repositories {
		if strings.EqualFold(repo.Repo, name) {
			return repo, true
		}
	}
	return config.Repository{}, false
}

// runRepository projects the stored selection onto current repository policy.
func (s *Scheduler) runRepository(run workflow.Run) (config.Repository, bool) {
	repo, ok := s.repository(run.Repository)
	return run.RoleSettings.Apply(repo), ok
}

// validateHarnessRole shares startup and claim-time adapter validation.
func validateHarnessRole(adapters map[string]harness.HarnessAdapter, role config.Role) error {
	adapter, ok := adapters[role.Agent]
	if !ok {
		return &fault.Error{Code: "config.invalid_agent", Message: "Unsupported phase harness: " + role.Agent}
	}
	capabilities := adapter.Capabilities()
	if !capabilities.StructuredOutput || !capabilities.SessionResume ||
		(role.Model != "" && !capabilities.ModelSelection) ||
		(role.Effort != "" && !capabilities.EffortSelection) ||
		(len(role.Skills) > 0 && !capabilities.SkillSelection) {
		return &fault.Error{Code: "harness.capability_unsupported", Message: "Phase harness requires structured output and session resume"}
	}
	return adapter.ValidateConfig(role)
}
