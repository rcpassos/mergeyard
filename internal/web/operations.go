package web

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// Operations is the shared runtime lifecycle boundary used by the local CLI.
type Operations interface {
	Runs(context.Context) ([]workflow.Run, error)
	Stop(context.Context, string) error
	Retry(context.Context, string) (workflow.Run, error)
	Watch(context.Context, string) (runner.SessionRef, error)
	Takeover(context.Context, string) (harness.InteractiveCommand, error)
}

// Status is a snapshot from the process owning the configured workspace.
type Status struct {
	Workspace string         `json:"workspace"`
	Paused    bool           `json:"paused"`
	Runs      []workflow.Run `json:"runs"`
	Token     string         `json:"csrf_token"`
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
	s.json(w, Status{Workspace: s.workspace, Paused: s.scheduler.Paused(), Runs: runs, Token: s.token})
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
	run, err := s.operations.Retry(r.Context(), r.PathValue("id"))
	if err != nil {
		s.operationError(w, r, err)
		return
	}
	s.json(w, run)
}
func (s *Server) takeoverRun(w http.ResponseWriter, r *http.Request) {
	command, err := s.operations.Takeover(r.Context(), r.PathValue("id"))
	if err != nil {
		s.operationError(w, r, err)
		return
	}
	s.json(w, command)
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
		case "takeover.unavailable", "takeover.operation_pending", "takeover.worktree_missing", "takeover.session_missing", "takeover.session_invalid", "takeover.harness_unknown", "takeover.process_ambiguous":
			status = http.StatusConflict
			message = failure.Message
		case "internal.run_not_found":
			status = http.StatusNotFound
		case "run.terminal", "phase.not_running", "retry.unavailable", "retry.process_running", "retry.stop_pending", "retry.pr_closed", "retry.worktree_dirty", "retry.head_diverged", "retry.head_ambiguous", "retry.pr_ambiguous", "retry.review_unrestored", "harness.session_resume_failed", "internal.run_conflict":
			status = http.StatusConflict
		}
	}
	slog.ErrorContext(r.Context(), "Run operation failed", "error_code", code, "action", r.URL.Path, "error", err)
	w.Header().Set("X-Mergeyard-Error-Code", code)
	http.Error(w, code+": "+message, status)
}
