package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/doctor"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
	"github.com/rcpassos/mergeyard/internal/workspace"
	"go.yaml.in/yaml/v3"
)

// DashboardOptions connects pages to the same persisted runtime and lifecycle
// controls used by dispatch. Diagnostics runs once in RunUpdates, outside HTTP.
type DashboardOptions struct {
	DB         *sql.DB
	Workspace  *workspace.Workspace
	Scheduler  *scheduler.Scheduler
	Config     config.Config
	ConfigPath string
	// CLIExecutable defaults to the running binary. Tests and embedded callers
	// can supply their Mergeyard CLI; copied commands must use its ownership gate.
	CLIExecutable string
	Diagnostics   func(context.Context) doctor.Report
}

type dashboard struct {
	DashboardOptions
	mu           sync.RWMutex
	queue        []scheduler.QueueIssue
	queueFailure []scheduler.Finding
	queueReady   bool
	queueError   string
	queueTime    string
	diagnostics  *doctor.Report
	cliDir       string
}

// NewDashboard adds runtime-backed pages to the local HTTP foundation.
func NewDashboard(bus *events.Bus, control Scheduler, options DashboardOptions) (*Server, error) {
	if options.DB == nil || options.Workspace == nil || options.Scheduler == nil {
		return nil, &fault.Error{Code: "internal.web_dependencies", Message: "Dashboard requires runtime storage, workspace, and scheduler"}
	}
	var err error
	if options.CLIExecutable == "" {
		options.CLIExecutable, err = os.Executable()
		if err != nil {
			return nil, &fault.Error{Code: "internal.web_dependencies", Message: "Cannot locate the Mergeyard CLI for takeover", Err: err}
		}
	}
	// Preserve the daemon's working directory for configurations whose workspace
	// path is relative. The CLI itself rebuilds the worktree path locally.
	cliDir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if options.ConfigPath != "" {
		options.ConfigPath, err = filepath.Abs(options.ConfigPath)
		if err != nil {
			return nil, err
		}
	}
	s, err := NewWithOperations(bus, control, options.Scheduler, options.Workspace.Root)
	if err != nil {
		return nil, err
	}
	s.dashboard = &dashboard{DashboardOptions: options, cliDir: cliDir}
	return s, nil
}

// RunUpdates refreshes GitHub discovery while paused or at capacity and emits
// transient SSE notifications, which are live-only and never persisted. The
// caller must cancel and join this before closing storage.
func (s *Server) RunUpdates(ctx context.Context) {
	if s.dashboard == nil {
		return
	}
	d := s.dashboard
	var checks sync.WaitGroup
	if d.Diagnostics != nil {
		checks.Add(1)
		go func() {
			defer checks.Done()
			checkCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			report := d.Diagnostics(checkCtx)
			d.mu.Lock()
			d.diagnostics = &report
			d.mu.Unlock()
			s.bus.Notify(ctx, events.Draft{Type: "diagnostics.updated", Payload: struct{}{}})
		}()
	}
	defer checks.Wait()
	interval := d.Config.PollInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	timer := time.NewTicker(interval)
	defer timer.Stop()
	for {
		refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		report, err := d.Scheduler.QueueReport(refreshCtx)
		cancel()
		d.applyQueueReport(report, err)
		if ctx.Err() != nil {
			return
		}
		if err != nil && !scheduler.GitHubFailure(err) {
			logDashboardError(ctx, "queue discovery", err)
		}
		s.bus.Notify(ctx, events.Draft{Type: "queue.updated", Payload: struct{}{}})
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

// applyQueueReport records one discovery pass. A stale report means the
// refresh budget expired while the shared listing was still being fetched, so
// the dashboard keeps its last successful snapshot and sync time instead of
// presenting the timeout as a discovery error.
func (d *dashboard) applyQueueReport(report scheduler.QueueSnapshot, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case report.Stale:
		// The scheduler returned its last usable pass. Keep the snapshot and
		// sync time already on screen; without a previous sync, show what it
		// returned but never present stale data as freshly synced.
		if d.queueTime == "" {
			d.queue, d.queueFailure, d.queueError = report.Issues, report.Failures, ""
		}
	case err == nil || scheduler.GitHubFailure(err):
		// A scoped GitHub failure only hides the affected repository. The
		// readable repositories still refresh, and the failure is shown
		// beside them until a later poll succeeds.
		d.queue, d.queueFailure, d.queueError = report.Issues, report.Failures, ""
		d.queueTime = time.Now().UTC().Format(time.RFC3339)
	default:
		d.queueError = errorCode(err, "internal.dashboard") + ": GitHub discovery failed. Check the application log; discovery will retry."
		if d.queueTime != "" {
			d.queueError += " Showing the last successful snapshot."
		}
	}
	d.queueReady = true
}

type runView struct {
	workflow.Run
	Title, Summary, IssueURL, PRURL, Branch, Worktree, SessionID string
	Agent, Model, Effort, Skills, ProcessSession, LogPath        string
	Attempt, Round                                               int
	ReviewPending, CanStop, CanRetry, Stopping                   bool
	CanHandback, CanTakeover                                     bool
	ResumeCommand, TakeoverPrerequisite                          string
	StoppedPublications                                          []scheduler.StoppedPublication
}

type issueView struct {
	Repository    string
	Issue         github.Issue
	Blockers      []github.Issue
	RefusalReason string
	Attention     bool
}

// queueGroup is one Queue group. Every open run and queued issue belongs to
// exactly one group, in display order.
type queueGroup struct {
	Key            string
	Name           string
	Empty          string
	Labels         []string
	ShowRepository bool
	Runs           []runView
	Issues         []issueView
	Failures       []scheduler.Finding
}

// Count is the number of rows a group holds.
func (g queueGroup) Count() int { return len(g.Runs) + len(g.Issues) + len(g.Failures) }

// Columns is the table width including the optional repository column.
func (g queueGroup) Columns() int {
	if g.ShowRepository {
		return 6
	}
	return 5
}

// Group indexes follow the same display order.
const (
	attentionIndex = iota
	mergeIndex
	runningIndex
	harnessIndex
	readyIndex
	blockedIndex
)

// Queue group keys match the filter chip values.
const (
	attentionGroup = "attention"
	mergeGroup     = "merge"
	runningGroup   = "running"
	harnessGroup   = "harness"
	readyGroup     = "ready"
	blockedGroup   = "blocked"
)

const defaultReadyLabel = "ready-for-agent"

type timelineEntry struct {
	ID      int64
	Type    string
	Time    string
	Details string
}

type pageData struct {
	Harnesses                         []scheduler.HarnessAvailability
	Page, Title, Path, Token          string
	Boot                              bool
	Paused                            bool
	Used, Limit                       int
	Groups                            []queueGroup
	QueueFilter                       string
	Recent                            []runView
	History                           []runView
	Run                               *runView
	Timeline                          []timelineEntry
	Log, LogMessage                   string
	QueueReady                        bool
	QueueError, QueueTime             string
	ConfigPath, ConfigText, Workspace string
	ConfigGroups                      []settingsGroup
	Doctor                            *doctor.Report
}

// ShowRepository reports whether the Queue shows which repository owns a row.
func (d pageData) ShowRepository() bool {
	return len(d.Groups) > 0 && d.Groups[0].ShowRepository
}

// GroupCount returns how many rows the named Queue group holds. The Dashboard
// reads its KPIs from the same groups the Queue renders, so the two cannot
// disagree.
func (d pageData) GroupCount(key string) int {
	for _, group := range d.Groups {
		if group.Key == key {
			return group.Count()
		}
	}
	return 0
}

// RunningCount, AttentionCount and MergeCount are the Dashboard KPIs.
func (d pageData) RunningCount() int   { return d.GroupCount(runningGroup) }
func (d pageData) AttentionCount() int { return d.GroupCount(attentionGroup) }
func (d pageData) MergeCount() int     { return d.GroupCount(mergeGroup) }

// NeedsYou returns the non-empty groups whose work waits on the owner, in
// Queue display order.
func (d pageData) NeedsYou() []queueGroup {
	var groups []queueGroup
	for _, key := range []string{attentionGroup, mergeGroup} {
		for _, group := range d.Groups {
			if group.Key == key && group.Count() > 0 {
				groups = append(groups, group)
			}
		}
	}
	return groups
}

// NeedsYouClear reports whether nothing waits on the owner.
func (d pageData) NeedsYouClear() bool { return len(d.NeedsYou()) == 0 }

// pageMeta names the page and title served at path. The locked boot layout
// uses it too, so a tab keeps the correct title and current-navigation state
// while the dashboard script completes the page.
func pageMeta(path string) (page, title string) {
	switch path {
	case "/":
		return "dashboard", "Dashboard"
	case "/queue":
		return "queue", "Queue"
	case "/history":
		return "history", "Run history"
	case "/settings":
		return "settings", "Settings & diagnostics"
	default:
		return "run", "Run detail"
	}
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	page, title := pageMeta(r.URL.Path)
	data := pageData{Token: s.token, Paused: s.scheduler.Paused(), Path: r.URL.Path, Page: page, Title: title}
	if page == "queue" {
		data.QueueFilter = queueGroupFilter(r.URL.Query().Get("group"))
		if data.QueueFilter != "" {
			// A filtered Queue keeps its query across live swaps.
			data.Path = r.URL.RequestURI()
		}
	}
	if s.dashboard != nil {
		if err := s.dashboard.populate(r.Context(), &data, r.PathValue("id")); err != nil {
			s.respondError(w, r, err, "internal.dashboard_data", http.StatusInternalServerError, "Could not load dashboard data. Check the application log.")
			return
		}
	} else {
		data.Groups = emptyGroups()
		data.ConfigText = "Runtime configuration is not connected."
		if data.Page == "run" {
			s.respondError(w, r, &fault.Error{Code: "internal.run_not_found"}, "internal.run_not_found", http.StatusNotFound, "Run not found")
			return
		}
	}
	name := "layout"
	if r.Header.Get("HX-Request") == "true" {
		// A swap replaces the page content and refreshes the header pills.
		name = "swap"
	}
	w.Header().Add("Vary", "HX-Request")
	w.Header().Add("Vary", DashboardSessionHeader)
	s.renderData(w, name, data)
}

func (d *dashboard) populate(ctx context.Context, data *pageData, id string) error {
	var err error
	data.Harnesses, err = d.Scheduler.HarnessAvailability(ctx)
	if err != nil {
		return err
	}
	data.Limit = d.Config.Concurrency
	d.mu.RLock()
	queue := append([]scheduler.QueueIssue(nil), d.queue...)
	queueFailures := append([]scheduler.Finding(nil), d.queueFailure...)
	data.QueueReady, data.QueueError, data.QueueTime = d.queueReady, d.queueError, d.queueTime
	var report *doctor.Report
	if d.diagnostics != nil {
		// A published report is never mutated; newDoctorReport clones its
		// slices before presenting them.
		copied := *d.diagnostics
		report = &copied
	}
	d.mu.RUnlock()
	failures := append(d.Scheduler.ReconcileFailures(), queueFailures...)
	if data.Page == "settings" {
		data.ConfigPath, data.Workspace = d.ConfigPath, d.Workspace.Root
		data.ConfigGroups = settingsGroups(d.Config)
		cfg, err := yaml.Marshal(d.Config)
		if err != nil {
			return err
		}
		data.ConfigText = string(cfg)
		if report != nil {
			data.Doctor = newDoctorReport(*report)
		}
		return nil
	}
	if data.Page == "history" {
		data.History, err = d.endedRuns(ctx)
		return err
	}
	runs, err := d.runs(ctx, id)
	if err != nil {
		return err
	}
	if data.Page == "run" {
		if len(runs) == 0 {
			return &fault.Error{Code: "internal.run_not_found", Message: "Run not found"}
		}
		data.Run = &runs[0]
		// A stopped publication keeps its reason until a human requeues it.
		data.Run.StoppedPublications, err = d.Scheduler.StoppedPublications(ctx, id)
		if err != nil {
			return err
		}
		data.Timeline, err = d.timeline(ctx, id)
		if err != nil {
			return err
		}
		data.Log, data.LogMessage = logTail(d.Workspace.Root, data.Run.LogPath)
		return nil
	}
	grouped := groupQueue(runs, queue, uniqueFailures(failures), queuePresentationFor(d.Config))
	data.Groups, data.Recent, data.Used = grouped.Groups, grouped.Recent, grouped.Used
	return nil
}

// groupedQueue is the dashboard's queue after every open run and queued issue
// has been placed in its group, plus the ended runs and slots it accounts for.
type groupedQueue struct {
	Groups []queueGroup
	Recent []runView
	Used   int
}

// unqueuedGroup marks a run that has ended and left the Queue.
const unqueuedGroup = -1

// recentEndedLimit bounds the Dashboard's Recently ended list. The dedicated
// history page lists every ended run.
const recentEndedLimit = 10

// queuePresentation is the configuration-derived shape of the Queue table.
type queuePresentation struct {
	ShowRepository bool
	ReadyLabels    []string
}

// queuePresentationFor reads the table shape from the configured repositories.
// The repository column depends on every configured repository because a
// disabled repository can still own retained runs and failures, while Ready
// labels come only from the repositories that can queue new work.
func queuePresentationFor(cfg config.Config) queuePresentation {
	presentation := queuePresentation{ShowRepository: len(cfg.Repositories) > 1}
	seen := map[string]bool{}
	for _, repo := range cfg.Repositories {
		if !repo.Enabled || repo.Labels.Ready == "" || seen[repo.Labels.Ready] {
			continue
		}
		seen[repo.Labels.Ready] = true
		presentation.ReadyLabels = append(presentation.ReadyLabels, repo.Labels.Ready)
	}
	if len(presentation.ReadyLabels) == 0 {
		presentation.ReadyLabels = []string{defaultReadyLabel}
	}
	return presentation
}

// groupQueue sorts runs and queued issues into the six Queue groups. A run in
// the queue owns its issue, so a stale queue snapshot never duplicates it, and
// ended runs leave the queue for recent history. Concurrency accounting is
// unchanged: every open run except ready-to-merge consumes a slot.
func groupQueue(runs []runView, queue []scheduler.QueueIssue, failures []scheduler.Finding, presentation queuePresentation) groupedQueue {
	groups := emptyGroups()
	for index := range groups {
		groups[index].ShowRepository = presentation.ShowRepository
	}
	groups[readyIndex].Labels = presentation.ReadyLabels
	groups[attentionIndex].Failures = failures
	result := groupedQueue{Groups: groups}
	queued := map[string]bool{}
	for _, run := range runs {
		index := groupForRun(run)
		if index == unqueuedGroup {
			if len(result.Recent) < recentEndedLimit {
				result.Recent = append(result.Recent, run)
			}
			continue
		}
		queued[queueIssueKey(run.Repository, run.IssueNumber)] = true
		if !run.State.Terminal() && run.State != workflow.ReadyToMerge {
			result.Used++
		}
		result.Groups[index].Runs = append(result.Groups[index].Runs, run)
	}
	for _, item := range queue {
		if queued[queueIssueKey(item.Repository, item.Issue.Number)] {
			continue
		}
		issue := issueView{
			Repository: item.Repository, Issue: item.Issue, Blockers: item.Blockers,
			RefusalReason: item.RefusalReason, Attention: item.Attention,
		}
		index := issueGroup(issue)
		result.Groups[index].Issues = append(result.Groups[index].Issues, issue)
	}
	return result
}

// groupForRun returns a run's group, or unqueuedGroup when it has ended. A
// merge can be observed before the owned phase exits, so cleanup state can
// keep even a terminal run in Needs attention.
func groupForRun(run runView) int {
	if run.Merge != nil && run.Merge.Pending() {
		return attentionIndex
	}
	if run.Ended() {
		return unqueuedGroup
	}
	switch run.State {
	case workflow.NeedsAttention:
		return attentionIndex
	case workflow.ReadyToMerge:
		return mergeIndex
	case workflow.WaitingForHarness:
		return harnessIndex
	default:
		// CLAIMING, PREPARING, ACTIVE, WAITING_FOR_CI and MANUAL all run.
		return runningIndex
	}
}

// issueGroup places an unclaimed issue using the same precedence as its badge.
func issueGroup(issue issueView) int {
	switch classifyIssue(issue) {
	case refusedIssue, blockedIssue:
		return blockedIndex
	case attentionIssue:
		return attentionIndex
	default:
		return readyIndex
	}
}

func queueIssueKey(repository string, number int) string {
	return fmt.Sprintf("%s/%d", strings.ToLower(repository), number)
}

// queueGroupFilter keeps a known filter chip selected and ignores anything
// else in the query, so a hand-edited URL still renders the whole queue.
func queueGroupFilter(value string) string {
	for _, group := range emptyGroups() {
		if group.Key == value {
			return value
		}
	}
	return ""
}

func emptyGroups() []queueGroup {
	return []queueGroup{
		{Key: attentionGroup, Name: "Needs attention", Empty: "Nothing needs you."},
		{Key: mergeGroup, Name: "Waiting for your merge", Empty: "Nothing is waiting for your merge."},
		{Key: runningGroup, Name: "Running", Empty: "No runs are in progress."},
		{Key: harnessGroup, Name: "Waiting for harness", Empty: "No runs are waiting for a harness."},
		{Key: readyGroup, Name: "Ready", Labels: []string{defaultReadyLabel}},
		{Key: blockedGroup, Name: "Blocked", Empty: "No issues are blocked."},
	}
}

// uniqueFailures keeps the most recent discovery and reconcile views of the
// same scoped failure from producing duplicate dashboard rows.
func uniqueFailures(failures []scheduler.Finding) []scheduler.Finding {
	seen := map[string]bool{}
	var unique []scheduler.Finding
	for _, failure := range failures {
		key := failure.Key()
		if seen[key] {
			continue
		}
		seen[key] = true
		unique = append(unique, failure)
	}
	return unique
}

func (d *dashboard) runs(ctx context.Context, id string) ([]runView, error) {
	observations, err := workflow.ObserveRecentRuns(ctx, d.DB, id)
	if err != nil {
		return nil, err
	}
	runs, err := d.runViews(observations)
	if err != nil {
		return nil, err
	}
	for i := range runs {
		runs[i].Stopping = runs[i].Stopping && runs[i].Merge == nil
		runs[i].CanHandback = runs[i].State == workflow.Manual && runs[i].TakeoverStatus == workflow.TakeoverManual && !runs[i].Stopping && runs[i].Merge == nil
		runs[i].CanRetry, err = d.Scheduler.RetryEligible(ctx, runs[i].Run)
		if err != nil {
			return nil, err
		}

		// Use the implementer's identity even when a reviewer owns the phase.
		if runs[i].State == workflow.Manual && runs[i].TakeoverStatus == workflow.TakeoverManual {
			_, err := d.Scheduler.ManualCommand(ctx, runs[i].ID)
			if err == nil && d.ConfigPath != "" {
				launcher := harness.InteractiveCommand{Executable: d.CLIExecutable, Dir: d.cliDir,
					Args: []string{"--config", d.ConfigPath, "takeover", "--", runs[i].ID}}
				runs[i].ResumeCommand = launcher.ShellCommand()
			} else if err == nil {
				runs[i].TakeoverPrerequisite = "Resume command unavailable. Start the dashboard with its configuration path so takeover can reach the owning workspace."
			} else if runs[i].PendingHandback() != nil {
				runs[i].TakeoverPrerequisite = "Handback publication is pending. Finish handback or Stop before resuming the implementer."
			} else {
				runs[i].TakeoverPrerequisite = "Resume command unavailable. Inspect the preserved worktree and implementer identity before requesting takeover again."
			}
		} else if !runs[i].State.Terminal() && runs[i].State != workflow.Manual && runs[i].Merge == nil && !runs[i].Stopping && runs[i].PendingRetry() == nil && runs[i].PendingHandback() == nil {
			switch {
			case runs[i].Worktree == "":
				runs[i].TakeoverPrerequisite = "Takeover needs a prepared worktree."
			case runs[i].Implementer == nil || runs[i].Implementer.SessionID == "":
				runs[i].TakeoverPrerequisite = "Takeover needs the implementer's session identity. Wait for it to be recorded; a new conversation will not be created."
			default:
				runs[i].CanTakeover = true
			}
		}
	}
	return runs, nil
}

// endedRuns returns every run that has left the Queue, newest first.
func (d *dashboard) endedRuns(ctx context.Context) ([]runView, error) {
	observations, err := workflow.ObserveEndedRuns(ctx, d.DB)
	if err != nil {
		return nil, err
	}
	return d.runViews(observations)
}

// runViews presents persisted observations. Action eligibility is added only
// for pages that offer actions.
func (d *dashboard) runViews(observations []workflow.RunObservation) ([]runView, error) {
	var runs []runView
	for _, observation := range observations {
		run := runView{
			Run:    observation.Run,
			Branch: observation.Branch, Worktree: observation.Worktree,
			Agent: observation.Agent, SessionID: observation.SessionID,
			PRURL: observation.PRURL, Attempt: observation.Attempt, Round: observation.Round,
			Model: observation.Model, Effort: observation.Effort,
			ProcessSession: observation.ProcessSession, LogPath: observation.LogPath,
			Stopping: observation.StopRequested,
		}
		issueJSON, skillsJSON := observation.IssueJSON, observation.SkillsJSON
		var issue github.Issue
		if err := json.Unmarshal([]byte(issueJSON), &issue); err != nil {
			return nil, err
		}
		run.Title, run.Summary = issue.Title, summary(issue.Body)
		run.IssueURL = fmt.Sprintf("https://github.com/%s/issues/%d", run.Repository, run.IssueNumber)
		if run.PRURL == "" && run.PRNumber > 0 {
			run.PRURL = fmt.Sprintf("https://github.com/%s/pull/%d", run.Repository, run.PRNumber)
		}
		if run.Attempt > 0 {
			if skillsJSON == "" {
				run.Skills = "Not recorded for this older attempt"
			} else {
				var skills []string
				if err := json.Unmarshal([]byte(skillsJSON), &skills); err != nil {
					return nil, err
				}
				run.Skills = strings.Join(skills, ", ")
			}
		} else {
			for _, repo := range d.Config.Repositories {
				if strings.EqualFold(repo.Repo, run.Repository) {
					repo = run.RoleSettings.Apply(repo)
					role := repo.Implementer
					if run.Phase == workflow.Review {
						role = repo.Reviewer
					}
					if run.Agent == "" {
						run.Agent = role.Agent
					}
					if run.Model == "" {
						run.Model = role.Model
					}
					if run.Effort == "" {
						run.Effort = role.Effort
					}
					run.Skills = strings.Join(role.Skills, ", ")
					break
				}
			}
		}

		run.ReviewPending = run.State == workflow.Active && run.Phase == workflow.Review && run.Attempt == 0 && run.PRURL != ""
		if run.Phase == workflow.Review || run.Phase == workflow.Fix {
			run.Round = run.ReviewRound
		}
		run.CanStop = !run.State.Terminal()
		run.Stopping = run.Stopping && run.CanStop
		runs = append(runs, run)
	}
	return runs, nil
}

func summary(body string) string {
	runes := []rune(body)
	if len(runes) > 600 {
		return string(runes[:600]) + "…"
	}
	return body
}

func (d *dashboard) timeline(ctx context.Context, id string) ([]timelineEntry, error) {
	rows, err := d.DB.QueryContext(ctx, "SELECT id,type,created_at,payload_json FROM events WHERE run_id=? ORDER BY id DESC LIMIT 100", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var timeline []timelineEntry
	for rows.Next() {
		var entry timelineEntry
		if err := rows.Scan(&entry.ID, &entry.Type, &entry.Time, &entry.Details); err != nil {
			return nil, err
		}
		timeline = append(timeline, entry)
	}
	return timeline, rows.Err()
}

func (s *Server) stopRunPage(w http.ResponseWriter, r *http.Request) {
	if s.dashboard == nil {
		s.respondError(w, r, &fault.Error{Code: "internal.run_not_found"}, "internal.run_not_found", http.StatusNotFound, "Run not found")
		return
	}
	if err := s.dashboard.Scheduler.Stop(r.Context(), r.PathValue("id")); err != nil {
		s.actionError(w, r, err)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		r.URL.Path = "/runs/" + r.PathValue("id")
		s.page(w, r)
		return
	}
	http.Redirect(w, r, "/runs/"+r.PathValue("id"), http.StatusSeeOther)
}

func (s *Server) retryRunPage(w http.ResponseWriter, r *http.Request) {
	if s.dashboard == nil {
		s.respondError(w, r, &fault.Error{Code: "internal.run_not_found"}, "internal.run_not_found", http.StatusNotFound, "Run not found")
		return
	}
	if _, err := s.operations.Retry(r.Context(), r.PathValue("id"), r.PostForm.Get("note")); err != nil {
		s.operationError(w, r, err)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		r.URL.Path = "/runs/" + r.PathValue("id")
		s.page(w, r)
		return
	}
	http.Redirect(w, r, "/runs/"+r.PathValue("id"), http.StatusSeeOther)
}

func (s *Server) handbackRunPage(w http.ResponseWriter, r *http.Request) {
	if s.dashboard == nil {
		s.respondError(w, r, &fault.Error{Code: "internal.run_not_found"}, "internal.run_not_found", http.StatusNotFound, "Run not found")
		return
	}
	if _, err := s.operations.Handback(r.Context(), r.PathValue("id")); err != nil {
		s.operationError(w, r, err)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		r.URL.Path = "/runs/" + r.PathValue("id")
		s.page(w, r)
		return
	}
	http.Redirect(w, r, "/runs/"+r.PathValue("id"), http.StatusSeeOther)
}

func (s *Server) takeoverRunPage(w http.ResponseWriter, r *http.Request) {
	if s.dashboard == nil {
		s.respondError(w, r, &fault.Error{Code: "internal.run_not_found"}, "internal.run_not_found", http.StatusNotFound, "Run not found")
		return
	}
	if _, err := s.operations.Takeover(r.Context(), r.PathValue("id")); err != nil {
		s.operationError(w, r, err)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		r.URL.Path = "/runs/" + r.PathValue("id")
		s.page(w, r)
		return
	}
	http.Redirect(w, r, "/runs/"+r.PathValue("id"), http.StatusSeeOther)
}

// requeueRunPage returns the run's stopped publications to the queue.
func (s *Server) requeueRunPage(w http.ResponseWriter, r *http.Request) {
	if s.dashboard == nil {
		s.respondError(w, r, &fault.Error{Code: "internal.run_not_found"}, "internal.run_not_found", http.StatusNotFound, "Run not found")
		return
	}
	if _, err := s.operations.RequeuePublications(r.Context(), r.PathValue("id")); err != nil {
		s.operationError(w, r, err)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		r.URL.Path = "/runs/" + r.PathValue("id")
		s.page(w, r)
		return
	}
	http.Redirect(w, r, "/runs/"+r.PathValue("id"), http.StatusSeeOther)
}

func (s *Server) output(w http.ResponseWriter, r *http.Request) {
	if s.dashboard == nil {
		s.respondError(w, r, &fault.Error{Code: "internal.run_not_found"}, "internal.run_not_found", http.StatusNotFound, "Run not found")
		return
	}
	runs, err := s.dashboard.runs(r.Context(), r.PathValue("id"))
	if err != nil {
		s.respondError(w, r, err, "internal.dashboard_output", http.StatusInternalServerError, "Could not load phase output")
		return
	}
	if len(runs) == 0 {
		s.respondError(w, r, &fault.Error{Code: "internal.run_not_found"}, "internal.run_not_found", http.StatusNotFound, "Run not found")
		return
	}
	data := pageData{Run: &runs[0]}
	data.Log, data.LogMessage = logTail(s.dashboard.Workspace.Root, runs[0].LogPath)
	s.renderData(w, "phase-output", data)
}
