// Package scheduler dispatches eligible issues and advances the bounded implementation, review, and fix loop.
package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sort"
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
	ListOpenIssues(context.Context, string) ([]github.Issue, error)
	GetIssue(context.Context, string, int) (github.Issue, error)
	GetPullRequest(context.Context, string, int) (*github.PullRequest, error)
	UnresolvedBlockers(context.Context, string, int) ([]github.Issue, error)
	AddLabel(context.Context, string, int, string) error
	RemoveLabel(context.Context, string, int, string) error
	FindOpenPullRequest(context.Context, string, string) (*github.PullRequest, error)
	FindPullRequest(context.Context, string, string) (*github.PullRequest, error)
	CreateDraftPullRequest(context.Context, string, string, string, github.PullRequestContent) (*github.PullRequest, error)
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
	reportedFindings map[string]bool // protected by tick; unchanged findings emit once per scheduler lifetime
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
				adapter, ok := adapters[role.Agent]
				if !ok {
					return nil, &fault.Error{Code: "config.invalid_agent", Message: "Unsupported phase harness: " + role.Agent}
				}
				capabilities := adapter.Capabilities()
				if !capabilities.StructuredOutput || !capabilities.SessionResume ||
					(role.Model != "" && !capabilities.ModelSelection) ||
					(role.Effort != "" && !capabilities.EffortSelection) ||
					(len(role.Skills) > 0 && !capabilities.SkillSelection) {
					return nil, &fault.Error{Code: "harness.capability_unsupported", Message: "Phase harness requires structured output and session resume"}
				}
				if err := adapter.ValidateConfig(role); err != nil {
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
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM runs ORDER BY created_at, id")
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	runs := make([]workflow.Run, 0, len(ids))
	for _, id := range ids {
		run, err := s.workflow.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		run.RetryEligible, err = s.RetryEligible(ctx, run)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
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
	_, err := s.reconcile(ctx)
	if err != nil {
		return err
	}
	var failures []error
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if s.control.Paused() {
		return errors.Join(failures...)
	}
	runs, err := s.Runs(ctx)
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	slots := 0
	repoSlots := map[string]int{}
	existing := map[string]bool{}
	for _, run := range runs {
		if run.Merge != nil && run.Merge.Pending() {
			existing[issueKey(run.Repository, run.IssueNumber)] = true
		}
		if !run.State.Terminal() {
			existing[issueKey(run.Repository, run.IssueNumber)] = true
			if run.State != workflow.ReadyToMerge {
				slots++
				repoSlots[strings.ToLower(run.Repository)]++
			}
		}
	}
	for _, repo := range s.cfg.Repositories {
		if !repo.Enabled || slots >= s.cfg.Concurrency || s.control.Paused() {
			continue
		}
		gate, err := s.harnessGate(ctx, repo.Implementer.Agent)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if gate != nil {
			continue
		}
		limit := repo.Concurrency
		if limit <= 0 {
			limit = s.cfg.Concurrency
		}
		issues, err := s.deps.GitHub.ListOpenIssues(ctx, repo.Repo)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		// Do not reorder a slice owned by the adapter.
		issues = append([]github.Issue(nil), issues...)
		sort.Slice(issues, func(i, j int) bool {
			if issues[i].CreatedAt.Equal(issues[j].CreatedAt) {
				return issues[i].Number < issues[j].Number
			}
			return issues[i].CreatedAt.Before(issues[j].CreatedAt)
		})
		for _, issue := range issues {
			if s.control.Paused() || slots >= s.cfg.Concurrency || repoSlots[strings.ToLower(repo.Repo)] >= limit {
				break
			}
			if issue.State != github.Open || !hasLabel(issue, repo.Labels.Ready) || hasLabel(issue, repo.Labels.Running) || hasLabel(issue, repo.Labels.NeedsAttention) || existing[issueKey(repo.Repo, issue.Number)] {
				continue
			}
			blockers, err := s.deps.GitHub.UnresolvedBlockers(ctx, repo.Repo, issue.Number)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if len(blockers) > 0 {
				continue
			}
			s.control.mu.Lock()
			if s.control.paused {
				s.control.mu.Unlock()
				break
			}
			run, err := s.workflow.Transition(ctx, uuid.NewString(), workflow.Request{Trigger: workflow.IssueClaimed, Repository: repo.Repo, IssueNumber: issue.Number})
			s.control.mu.Unlock()
			if err != nil {
				failures = append(failures, err)
				continue
			}
			slots++
			repoSlots[strings.ToLower(repo.Repo)]++
			existing[issueKey(repo.Repo, issue.Number)] = true
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
