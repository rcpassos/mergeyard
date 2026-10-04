// Package scheduler dispatches eligible issues and advances the M1 implement flow.
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
	"github.com/rcpassos/mergeyard/internal/app"
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
	UnresolvedBlockers(context.Context, string, int) ([]github.Issue, error)
	AddLabel(context.Context, string, int, string) error
	RemoveLabel(context.Context, string, int, string) error
	FindOpenPullRequest(context.Context, string, string) (*github.PullRequest, error)
	CreateDraftPullRequest(context.Context, string, string, string, github.PullRequestContent) (*github.PullRequest, error)
}

// Git is the managed Git boundary; implementations must preserve run ownership.
type Git interface {
	Prepare(context.Context, managedgit.PrepareRequest) (managedgit.Run, error)
	CommitAndPush(context.Context, managedgit.Run, managedgit.Phase) (managedgit.CommitResult, error)
}

type Dependencies struct {
	GitHub GitHub
	Git    Git
	Runner runner.Runner
	// Env is the complete harness environment; callers explicitly select it.
	Env map[string]string
}

type Scheduler struct {
	cfg       config.Config
	db        *sql.DB
	bus       *events.Bus
	workflow  *workflow.Workflow
	workspace *workspace.Workspace
	deps      Dependencies
	claude    *harness.Claude
	tick      sync.Mutex
	pause     sync.Mutex
	paused    atomic.Bool
	running   atomic.Bool
}

// New requires an exclusively owned runtime workspace. Startup wiring lives in #14.
func New(cfg config.Config, runtime *app.Runtime, deps Dependencies) (*Scheduler, error) {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 30 * time.Second
	}
	if cfg.Concurrency < 1 {
		return nil, &fault.Error{Code: "config.invalid_concurrency", Message: "Scheduler concurrency must be positive"}
	}
	claude := harness.NewClaude(cfg.Agents.Claude)
	for _, repo := range cfg.Repositories {
		if repo.Enabled {
			if err := claude.ValidateConfig(repo.Implementer); err != nil {
				return nil, err
			}
		}
	}
	if deps.GitHub == nil {
		deps.GitHub = github.New(nil)
	}
	if deps.Git == nil {
		deps.Git = managedgit.New(runtime.Workspace)
	}
	if deps.Runner == nil {
		deps.Runner = runner.NewLocal(runner.Options{})
	}
	env := make(map[string]string, len(deps.Env))
	for k, v := range deps.Env {
		env[k] = v
	}
	deps.Env = env
	return &Scheduler{cfg: cfg, db: runtime.DB, bus: runtime.Events, workflow: runtime.Workflow, workspace: runtime.Workspace, deps: deps, claude: claude}, nil
}

// Pause only stops new claims. Existing implement attempts continue on ticks.
func (s *Scheduler) Pause(ctx context.Context, paused bool) error {
	s.pause.Lock()
	defer s.pause.Unlock()
	if s.paused.Load() == paused {
		return nil
	}
	event := "scheduler.resumed"
	if paused {
		event = "scheduler.paused"
	}
	if _, err := s.bus.Publish(ctx, events.Draft{Type: event, Payload: map[string]bool{"paused": paused}}); err != nil {
		return err
	}
	s.paused.Store(paused)
	return nil
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
	for {
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "scheduler tick", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// Runs returns the persisted lifecycle snapshots, including the M1 endpoint.
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
	runs, err := s.Runs(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, run := range runs {
		repo, ok := s.repository(run.Repository)
		if !ok {
			continue
		}
		if err := s.advance(ctx, repo, run); err != nil {
			failures = append(failures, err)
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if s.paused.Load() {
		return errors.Join(failures...)
	}
	runs, err = s.Runs(ctx)
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	slots := 0
	repoSlots := map[string]int{}
	existing := map[string]bool{}
	for _, run := range runs {
		if !run.State.Terminal() {
			existing[issueKey(run.Repository, run.IssueNumber)] = true
			if run.State != workflow.ReadyToMerge {
				slots++
				repoSlots[strings.ToLower(run.Repository)]++
			}
		}
	}
	for _, repo := range s.cfg.Repositories {
		if !repo.Enabled || slots >= s.cfg.Concurrency || s.paused.Load() {
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
			if s.paused.Load() || slots >= s.cfg.Concurrency || repoSlots[strings.ToLower(repo.Repo)] >= limit {
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
			s.pause.Lock()
			if s.paused.Load() {
				s.pause.Unlock()
				break
			}
			run, err := s.workflow.Transition(ctx, uuid.NewString(), workflow.Request{Trigger: workflow.IssueClaimed, Repository: repo.Repo, IssueNumber: issue.Number})
			s.pause.Unlock()
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
