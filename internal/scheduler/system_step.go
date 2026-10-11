package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/sessions"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// systemStepRequest supplies commands from repository configuration only.
// TargetSHA pins a check invocation to its commit; setup leaves it empty.
type systemStepRequest struct {
	Step      string
	Commands  []string
	TargetSHA string
	Retry     bool
}

func (s *Scheduler) setup(ctx context.Context, repo config.Repository, run workflow.Run, gitRun managedgit.Run) (bool, error) {
	if len(repo.Setup) == 0 {
		return true, nil
	}
	v, done, err := s.systemStep(ctx, repo, run, gitRun, systemStepRequest{Step: "setup", Commands: repo.Setup, Retry: true})
	if err != nil {
		return false, err
	}
	if !done {
		return false, nil
	}
	if v.Status != "succeeded" {
		return false, &fault.Error{Code: "workspace.setup_failed", Message: "Workspace setup failed: " + v.Error + "\n" + v.Output}
	}
	return true, nil
}

// systemStep starts or observes one journaled command list through the phase runner.
// The same metadata, wrapper, and process inventory support setup and checks.
func (s *Scheduler) systemStep(ctx context.Context, repo config.Repository, run workflow.Run, gitRun managedgit.Run, req systemStepRequest) (*workflow.SystemStepSnapshot, bool, error) {
	identity, err := worktreeIdentity(gitRun.Path)
	if err != nil {
		return nil, false, err
	}
	steps, err := workflow.LoadSystemSteps(ctx, s.db, run.ID)
	if err != nil {
		return nil, false, err
	}
	var last *workflow.SystemStepSnapshot
	number := 1
	for i := range steps {
		v := &steps[i]
		if v.Step == req.Step && v.Phase == run.Phase && v.Round == phaseRound(run, run.Phase) {
			number = v.Attempt + 1
			last = v
		}
	}
	if last == nil || last.WorktreeIdentity != identity || last.TargetSHA != req.TargetSHA || (req.Retry && last.Status != "running" && last.Status != "succeeded") {
		return s.startSystemStep(ctx, repo, run, gitRun, req, number)
	}
	if last.Status != "running" {
		return last, true, nil
	}
	ref := runner.SessionRef{Name: last.ProcessSession, PhaseDir: last.PhaseDir}
	status, err := s.deps.Runner.SessionStatus(ctx, ref)
	if err != nil {
		return nil, false, err
	}
	if status.State == runner.SessionRunning {
		return last, false, nil
	}
	if status.State == runner.SessionMissing || status.ExitCode == nil {
		// A crash before launch, or a forced exit without metadata, cannot establish
		// success. Verify owned descendants are gone before rerunning idempotent work.
		if err := s.deps.Runner.StopSession(ctx, ref); err != nil {
			return nil, false, err
		}
		last.Status, last.Error = "failed", "Unknown outcome; commands will run again"
		if err := s.saveSystemStep(ctx, run.ID, last); err != nil {
			return nil, false, err
		}
		return s.startSystemStep(ctx, repo, run, gitRun, req, number)
	}
	last.ExitCode, last.TimedOut = status.ExitCode, status.TimedOut
	last.Status = "succeeded"
	if *status.ExitCode != 0 {
		last.Status, last.Error = "failed", fmt.Sprintf("exit %d", *status.ExitCode)
	}
	if status.TimedOut {
		last.Error = "timed out after " + last.CommandTimeout
	}
	var output strings.Builder
	for _, name := range []string{"events.jsonl", "stderr.log"} {
		data, err := s.deps.Runner.ReadFile(ctx, filepath.Join(last.PhaseDir, name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, false, err
		}
		output.Write(data)
	}
	last.Output = excerpt(output.String())
	if err := s.saveSystemStep(ctx, run.ID, last); err != nil {
		return nil, false, err
	}
	return last, true, nil
}

func excerpt(output string) string {
	const limit = 4096
	if len(output) > limit {
		return "[earlier output omitted]\n" + output[len(output)-limit:]
	}
	return output
}

func (s *Scheduler) startSystemStep(ctx context.Context, repo config.Repository, run workflow.Run, gitRun managedgit.Run, req systemStepRequest, number int) (*workflow.SystemStepSnapshot, bool, error) {
	identity, err := worktreeIdentity(gitRun.Path)
	if err != nil {
		return nil, false, err
	}
	timeout := repo.CommandTimeout
	if timeout <= 0 {
		timeout = 20 * time.Minute
	}
	dir := filepath.Join(s.workspace.Root, "runs", run.ID, "phases", fmt.Sprintf("%s-%d-%d", req.Step, phaseRound(run, run.Phase), number))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, false, err
	}
	if err := sessions.RequireProcessJournal(dir); err != nil {
		return nil, false, err
	}
	input := filepath.Join(dir, "commands.sh")
	// One login shell shares setup state between list entries; stop on the first
	// failing command or pipeline. No command text is taken from issues or PRs.
	script := "set -e\nset -o pipefail\n" + strings.Join(req.Commands, "\n") + "\n"
	if err := s.deps.Runner.WriteFile(ctx, input, []byte(script), 0600); err != nil {
		return nil, false, err
	}
	session := runner.SessionRequest{Timeout: timeout, RunID: run.ID, Phase: req.Step, Round: phaseRound(run, run.Phase), Attempt: number, PhaseDir: dir, Command: runner.ExecRequest{Executable: "/bin/bash", Args: []string{"--login", input}, Dir: gitRun.Path, Env: s.deps.Env}}
	v := &workflow.SystemStepSnapshot{ID: uuid.NewString(), Phase: run.Phase, Step: req.Step, Round: session.Round, Attempt: number, Commands: req.Commands, CommandTimeout: timeout.String(), TargetSHA: req.TargetSHA, WorktreeIdentity: identity, Status: "running", ProcessSession: sessions.Name(session), PhaseDir: dir}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, false, err
	}
	_, err = s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		_, err := tx.ExecContext(ctx, `INSERT INTO phase_attempts(id,run_id,phase,role,round,attempt,agent,status,process_session,input_path,log_path,step,system_step_json) VALUES (?,?,?,'system',?,?,'system','running',?,?,?,?,?)`, v.ID, run.ID, run.Phase, v.Round, v.Attempt, v.ProcessSession, input, filepath.Join(dir, "events.jsonl"), v.Step, string(data))
		return events.Draft{RunID: run.ID, Type: "system_step.started", Payload: v}, err
	})
	if err != nil {
		return nil, false, err
	}
	ref, err := s.deps.Runner.StartSession(ctx, session)
	if err != nil && ref.Name != "" && ctx.Err() == nil {
		return v, false, nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return v, false, ctx.Err()
		}
		v.Status, v.Error = "failed", err.Error()
		if err := s.saveSystemStep(ctx, run.ID, v); err != nil {
			return nil, false, err
		}
		return v, true, nil
	}
	return v, false, nil
}

func (s *Scheduler) saveSystemStep(ctx context.Context, id string, v *workflow.SystemStepSnapshot) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.bus.Commit(ctx, func(tx *sql.Tx) (events.Draft, error) {
		_, err := tx.ExecContext(ctx, `UPDATE phase_attempts SET status=?,exit_code=?,error=?,system_step_json=?,ended_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, v.Status, v.ExitCode, v.Error, string(data), v.ID)
		return events.Draft{RunID: id, Type: "system_step." + v.Status, Payload: v}, err
	})
	return err
}

func systemSession(run workflow.Run, name string) bool {
	for _, v := range run.SystemSteps {
		if v.ProcessSession == name {
			return true
		}
	}
	return false
}

// The worktree's Git pointer remains stable while commands and agents edit files.
// Its replacement identity prevents reusing a passed step after recreation.
func worktreeIdentity(path string) (string, error) {
	info, err := os.Stat(filepath.Join(path, ".git"))
	if err != nil {
		return "", err
	}
	stat := info.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("%d:%d:%d", stat.Dev, stat.Ino, info.ModTime().UnixNano()), nil
}
