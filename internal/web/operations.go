package web

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// Operations is the shared runtime lifecycle boundary used by the local CLI.
type Operations interface {
	Runs(context.Context) ([]workflow.Run, error)
	Stop(context.Context, string) error
	Retry(context.Context, string, ...string) (workflow.Run, error)
	Watch(context.Context, string) (runner.SessionRef, error)
	Handback(context.Context, string) (workflow.Run, error)
	Takeover(context.Context, string) (workflow.Run, error)
	RequeuePublications(context.Context, string) ([]scheduler.StoppedPublication, error)
}

// RequeueResult answers a requeue request with the publications it returned
// to the queue.
type RequeueResult struct {
	RunID        string                         `json:"run_id"`
	Requeued     int                            `json:"requeued"`
	Publications []scheduler.StoppedPublication `json:"publications,omitempty"`
}

// Status is a snapshot from the process owning the configured workspace.
type Status struct {
	ConfigPath string                          `json:"config_path,omitempty"`
	Config     *config.Config                  `json:"effective_config,omitempty"`
	Harnesses  []scheduler.HarnessAvailability `json:"harnesses,omitempty"`
	Workspace  string                          `json:"workspace"`
	Paused     bool                            `json:"paused"`
	Runs       []workflow.Run                  `json:"runs"`
	Token      string                          `json:"csrf_token"`
}

// NewWithOperations enables the CLI API on the same server as the dashboard.
func NewWithOperations(bus *events.Bus, scheduler Scheduler, operations Operations, workspace string) (*Server, error) {
	if operations == nil || workspace == "" {
		return nil, &fault.Error{Code: "internal.web_dependencies", Message: "CLI API requires runtime operations and workspace"}
	}
	return newServer(bus, scheduler, operations, workspace)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	runs, err := s.operations.Runs(r.Context())
	if err != nil {
		s.operationError(w, r, err)
		return
	}
	var availability []scheduler.HarnessAvailability
	if provider, ok := s.operations.(interface {
		HarnessAvailability(context.Context) ([]scheduler.HarnessAvailability, error)
	}); ok {
		availability, err = provider.HarnessAvailability(r.Context())
		if err != nil {
			s.operationError(w, r, err)
			return
		}
	}
	status := Status{Harnesses: availability, Workspace: s.workspace, Paused: s.scheduler.Paused(), Runs: runs, Token: s.token}
	if s.dashboard != nil {
		status.ConfigPath = s.dashboard.ConfigPath
		status.Config = &s.dashboard.Config
	}
	s.json(w, status)
}
func (s *Server) stopRun(w http.ResponseWriter, r *http.Request) {
	if err := s.operations.Stop(r.Context(), r.PathValue("id")); err != nil {
		s.operationError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) watchRun(w http.ResponseWriter, r *http.Request) {
	ref, err := s.operations.Watch(r.Context(), r.PathValue("id"))
	if err != nil {
		s.operationError(w, r, err)
		return
	}
	s.json(w, ref)
}
func (s *Server) retryRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.operations.Retry(r.Context(), r.PathValue("id"), r.PostForm.Get("note"))
	if err != nil {
		s.operationError(w, r, err)
		return
	}
	s.json(w, run)
}
func (s *Server) handbackRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.operations.Handback(r.Context(), r.PathValue("id"))
	if err != nil {
		s.operationError(w, r, err)
		return
	}
	s.json(w, run)
}
func (s *Server) takeoverRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.operations.Takeover(r.Context(), r.PathValue("id"))
	if err != nil {
		s.operationError(w, r, err)
		return
	}
	s.json(w, run)
}

// requeueRun returns the run's stopped publications to the queue, so a fixed
// cause is delivered again.
func (s *Server) requeueRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	publications, err := s.operations.RequeuePublications(r.Context(), id)
	if err != nil {
		s.operationError(w, r, err)
		return
	}
	s.json(w, RequeueResult{RunID: id, Requeued: len(publications), Publications: publications})
}

// dashboardSession mints a fresh one-time dashboard link for the CLI's open
// command. Only a caller that proved the workspace secret reaches it.
func (s *Server) dashboardSession(w http.ResponseWriter, r *http.Request) {
	s.json(w, struct {
		URL string `json:"url"`
	}{URL: "http://" + r.Host + "/?token=" + s.NewDashboardToken()})
}
func (s *Server) json(w http.ResponseWriter, value any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value)
}
func (s *Server) operationError(w http.ResponseWriter, r *http.Request, err error) {
	code := "internal.run_operation"
	status := http.StatusInternalServerError
	message := "Could not complete run operation. Check the application log for details."
	var failure *fault.Error
	if errors.As(err, &failure) && errorCodePattern.MatchString(failure.Code) {
		code = failure.Code
		switch code {
		case "harness.check_failed", "harness.check_waiting", "harness.unknown", "harness.probe_unavailable", "harness.credit_detection_unavailable", "handback.unavailable", "handback.operation_pending", "handback.process_ambiguous", "handback.review_unrestored", "handback.context_missing", "handback.git_unsupported", "handback.pr_ambiguous", "handback.pr_closed", "handback.git_ambiguous", "handback.publication_pending", "takeover.interactive_running", "takeover.unavailable", "takeover.operation_pending", "takeover.worktree_missing", "takeover.session_missing", "takeover.session_invalid", "takeover.harness_unknown", "takeover.process_ambiguous":
			status = http.StatusConflict
			message = failure.Message
		case "internal.run_not_found":
			status = http.StatusNotFound
		case "retry.invalid_note":
			status = http.StatusBadRequest
			message = failure.Message
		case "retry.note_conflict":
			status = http.StatusConflict
			message = failure.Message
		case "run.terminal", "phase.not_running", "retry.unavailable", "retry.process_running", "retry.stop_pending", "retry.pr_closed", "retry.worktree_dirty", "retry.head_diverged", "retry.head_ambiguous", "retry.pr_ambiguous", "retry.review_unrestored", "harness.session_resume_failed", "internal.run_conflict":
			status = http.StatusConflict
		}
	}
	slog.ErrorContext(r.Context(), "Run operation failed", "error_code", code, "action", r.URL.Path, "error", err)
	w.Header().Set("X-Mergeyard-Error-Code", code)
	http.Error(w, code+": "+message, status)
}

func (s *Server) checkHarness(w http.ResponseWriter, r *http.Request) {
	provider, ok := s.operations.(interface {
		CheckHarness(context.Context, string) (workflow.HarnessCheck, error)
	})
	if !ok {
		s.operationError(w, r, &fault.Error{Code: "harness.credit_detection_unavailable", Message: "Availability checks are unavailable in this runtime"})
		return
	}
	check, err := provider.CheckHarness(r.Context(), r.PathValue("harness"))
	if err != nil {
		s.operationError(w, r, err)
		return
	}
	if r.Header.Get("HX-Request") == "true" && s.dashboard != nil {
		r.URL.Path = "/"
		s.page(w, r)
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		s.json(w, check)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) plan(w http.ResponseWriter, r *http.Request) {
	provider, ok := s.operations.(interface {
		Plan(context.Context, string) (scheduler.Plan, error)
	})
	if !ok {
		http.Error(w, "Planning is unavailable", http.StatusNotImplemented)
		return
	}
	report, err := provider.Plan(r.Context(), r.URL.Query().Get("repo"))
	if err != nil {
		s.operationError(w, r, err)
		return
	}
	s.json(w, report)
}
