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
	mu          sync.RWMutex
	queue       []scheduler.QueueIssue
	queueReady  bool
	queueError  string
	queueTime   string
	diagnostics string
	cliDir      string
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
	s.dashboard = &dashboard{DashboardOptions: options, diagnostics: "Diagnostics have not been run.", cliDir: cliDir}
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
			var result strings.Builder
			report.Write(&result)
			d.mu.Lock()
			d.diagnostics = result.String()
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
		queue, err := d.Scheduler.Queue(refreshCtx)
		cancel()
		d.mu.Lock()
		if err == nil {
			d.queue, d.queueError = queue, ""
			d.queueTime = time.Now().UTC().Format(time.RFC3339)
		} else {
			d.queueError = errorCode(err, "internal.dashboard") + ": GitHub discovery failed. Check the application log; discovery will retry."
			if d.queueTime != "" {
				d.queueError += " Showing the last successful snapshot."
			}
		}
		d.queueReady = true
		d.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
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

type runView struct {
	workflow.Run
	Title, Summary, IssueURL, PRURL, Branch, Worktree, SessionID string
	Agent, Model, Effort, Skills, ProcessSession, LogPath        string
	Attempt, Round                                               int
	ReviewPending, CanStop, CanRetry, Stopping                   bool
	CanHandback, CanTakeover                                     bool
	ResumeCommand, TakeoverPrerequisite                          string
}

type issueView struct {
	Repository    string
	Issue         github.Issue
	Blockers      []github.Issue
	RefusalReason string
}

type queueSection struct {
	Name   string
	Runs   []runView
	Issues []issueView
}

const (
	runningSection = iota
	readySection
	blockedSection
	attentionSection
	draftPRSection
)

type timelineEntry struct{ Type, Time, Details string }

type pageData struct {
	Harnesses                                 []scheduler.HarnessAvailability
	RecoveryRuns                              []workflow.Run
	Page, Title, Path, Token                  string
	Boot                                      bool
	Paused                                    bool
	Used, Limit                               int
	Sections                                  []queueSection
	Recent                                    []runView
	Run                                       *runView
	Timeline                                  []timelineEntry
	Log, LogMessage                           string
	QueueReady                                bool
	QueueError, QueueTime                     string
	ConfigPath, ConfigText, Workspace, Doctor string
}

// pageMeta names the page and title served at path. The locked boot layout
// uses it too, so a tab keeps the correct title and current-navigation state
// while the dashboard script completes the page.
func pageMeta(path string) (page, title string) {
	switch path {
	case "/":
		return "dashboard", "Dashboard"
	case "/queue":
		return "queue", "Queue"
	case "/settings":
		return "settings", "Settings & diagnostics"
	default:
		return "run", "Run detail"
	}
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	page, title := pageMeta(r.URL.Path)
	data := pageData{Token: s.token, Paused: s.scheduler.Paused(), Path: r.URL.Path, Page: page, Title: title}
	if s.dashboard != nil {
		if err := s.dashboard.populate(r.Context(), &data, r.PathValue("id")); err != nil {
			s.respondError(w, r, err, "internal.dashboard_data", http.StatusInternalServerError, "Could not load dashboard data. Check the application log.")
			return
		}
	} else {
		data.Sections = emptySections()
		data.ConfigText, data.Doctor = "Runtime configuration is not connected.", "Diagnostics have not been run."
		if data.Page == "run" {
			s.respondError(w, r, &fault.Error{Code: "internal.run_not_found"}, "internal.run_not_found", http.StatusNotFound, "Run not found")
			return
		}
	}
	name := "layout"
	if r.Header.Get("HX-Request") == "true" {
		name = "content"
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
	data.QueueReady, data.QueueError, data.QueueTime = d.queueReady, d.queueError, d.queueTime
	data.Doctor = d.diagnostics
	d.mu.RUnlock()
	if data.Page == "settings" {
		data.ConfigPath, data.Workspace = d.ConfigPath, d.Workspace.Root
		cfg, err := yaml.Marshal(d.Config)
		data.ConfigText = string(cfg)
		if err != nil {
			return err
		}
		data.RecoveryRuns, err = d.Scheduler.Runs(ctx)
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
		data.Timeline, err = d.timeline(ctx, id)
		if err != nil {
			return err
		}
		data.Log, data.LogMessage = logTail(d.Workspace.Root, data.Run.LogPath)
		return nil
	}
	data.Sections = emptySections()
	existing := map[string]bool{}
	for _, run := range runs {
		key := fmt.Sprintf("%s/%d", strings.ToLower(run.Repository), run.IssueNumber)
		if !run.State.Terminal() {
			existing[key] = true
			if run.State != workflow.ReadyToMerge {
				data.Used++
			}
		}
		section := runningSection
		switch {
		case run.State == workflow.Completed && run.Merge != nil && run.Merge.Pending():
			section = attentionSection
		case run.State.Terminal():
			if len(data.Recent) < 20 {
				data.Recent = append(data.Recent, run)
			}
			continue
		case run.State == workflow.NeedsAttention || run.State == workflow.Manual:
			section = attentionSection
		case run.ReviewPending:
			section = draftPRSection
		}
		data.Sections[section].Runs = append(data.Sections[section].Runs, run)
	}
	for _, item := range queue {
		if existing[fmt.Sprintf("%s/%d", strings.ToLower(item.Repository), item.Issue.Number)] {
			continue
		}
		section := readySection
		if item.Attention {
			section = attentionSection
		} else if len(item.Blockers) > 0 {
			section = blockedSection
		}
		data.Sections[section].Issues = append(data.Sections[section].Issues, issueView{Repository: item.Repository, Issue: item.Issue, Blockers: item.Blockers, RefusalReason: item.RefusalReason})
	}
	return nil
}

func emptySections() []queueSection {
	return []queueSection{{Name: "Running"}, {Name: "Ready"}, {Name: "Blocked"}, {Name: "Needs attention"}, {Name: "Draft PRs · review pending"}}
}

func (d *dashboard) runs(ctx context.Context, id string) ([]runView, error) {
	observations, err := workflow.ObserveRecentRuns(ctx, d.DB, id)
	if err != nil {
		return nil, err
	}
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

func summary(body string) string {
	runes := []rune(body)
	if len(runes) > 600 {
		return string(runes[:600]) + "…"
	}
	return body
}

func (d *dashboard) timeline(ctx context.Context, id string) ([]timelineEntry, error) {
	rows, err := d.DB.QueryContext(ctx, "SELECT type,created_at,payload_json FROM events WHERE run_id=? ORDER BY id DESC LIMIT 100", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var timeline []timelineEntry
	for rows.Next() {
		var entry timelineEntry
		if err := rows.Scan(&entry.Type, &entry.Time, &entry.Details); err != nil {
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
