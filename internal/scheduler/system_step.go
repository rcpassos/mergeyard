package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/events"
	"github.com/rcpassos/mergeyard/internal/execution/durable"
	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/sessions"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// systemStepRequest supplies commands from repository configuration only.
// TargetSHA pins a check invocation to its commit; setup leaves it empty.
type systemStepRequest struct {
	OwnerID        string
	Step           string
	Commands       []string
	TargetSHA      string
	Retry          bool
	CommandTimeout time.Duration
}

func (s *Scheduler) setup(ctx context.Context, repo config.Repository, run workflow.Run, gitRun managedgit.Run) (bool, error) {
	if len(repo.Setup) == 0 && !runningSystemStep(run, "setup") {
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
		if v.Step == req.Step && v.Round == durable.AttemptRound(run, run.Phase) {
			// The session name contains the system step and round, so handback
			// and fix steps in that round share one physical number sequence.
			if v.Attempt >= number {
				number = v.Attempt + 1
			}
			if v.Phase == run.Phase {
				last = v
			}
		}
	}
	// Clearing a list cannot erase a durable execution which still needs
	// observation. Its saved commands and timeout remain the obligation until
	// that execution finishes; later phases use the current configuration.
	if last != nil && last.Status == "running" && len(req.Commands) == 0 {
		req.Commands = last.Commands
		var err error
		req.CommandTimeout, err = storedCommandTimeout(last)
		if err != nil {
			return nil, false, err
		}
	}
	commandsChanged := last != nil && (req.Step == "checks" || req.Step == "verify") && !slices.Equal(last.Commands, req.Commands)
	if last == nil || (commandsChanged && last.Status != "running") || last.WorktreeIdentity != identity || last.TargetSHA != req.TargetSHA || last.OwnerID != req.OwnerID || (req.Retry && last.Status != "running" && last.Status != "succeeded") {
		return s.startSystemStep(ctx, repo, run, gitRun, req, number)
	}
	if last.Status != "running" {
		if (last.Step == "checks" || last.Step == "verify") && last.Status == "succeeded" {
			failure := checkEvidenceError(last)
			if failure == "" && !last.PinnedTreeVerified {
				failure = "Checked workspace has no durable pinned-tree verification"
			}
			if failure == "" {
				if err := s.verifyCheckedTree(ctx, gitRun, last.TargetSHA); err != nil {
					failure = "Checked workspace differs from pinned commit: " + err.Error()
				}
			}
			if failure != "" {
				last.PinnedTreeVerified = false
				last.Status, last.Error = "failed", failure
				if err := s.saveSystemStep(ctx, run.ID, last); err != nil {
					return nil, false, err
				}
			}
		}
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
		// Unknown outcomes replay the original configured execution before a
		// replacement list can run, including when the current list is empty.
		req.Commands = last.Commands
		req.CommandTimeout, err = storedCommandTimeout(last)
		if err != nil {
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
	if last.Step == "checks" || last.Step == "verify" {
		last.Results, err = s.checkResults(ctx, last)
		if err != nil {
			return nil, false, err
		}
		if err := s.verifyCheckedTree(ctx, gitRun, last.TargetSHA); err != nil {
			last.Status, last.Error = "failed", "Workspace changed while checks ran: "+err.Error()
			last.PinnedTreeVerified = false
		} else {
			last.PinnedTreeVerified = true
		}
		if last.Status == "succeeded" {
			if failure := checkEvidenceError(last); failure != "" {
				last.Status, last.Error = "failed", failure
			}
		}
	}
	if err := s.saveSystemStep(ctx, run.ID, last); err != nil {
		return nil, false, err
	}
	if commandsChanged {
		return s.startSystemStep(ctx, repo, run, gitRun, req, number)
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
	if req.Step == "checks" || req.Step == "verify" {
		if err := s.verifyCheckedTree(ctx, gitRun, req.TargetSHA); err != nil {
			return nil, false, err
		}
	}
	identity, err := worktreeIdentity(gitRun.Path)
	if err != nil {
		return nil, false, err
	}
	timeout := req.CommandTimeout
	if timeout <= 0 {
		timeout = repo.CommandTimeout
	}
	if timeout <= 0 {
		timeout = 20 * time.Minute
	}
	dir := filepath.Join(s.workspace.Root, "runs", run.ID, "phases", fmt.Sprintf("%s-%d-%d", req.Step, durable.AttemptRound(run, run.Phase), number))
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
	if req.Step == "checks" || req.Step == "verify" {
		script = checksScript(dir, req.Commands)
	}
	// Keep login startup state, but contain explicit exits and the check EXIT
	// recorder in one shared subshell. The outer login shell finishes at EOF,
	// so logout hooks cannot overwrite the configured commands' exit status.
	script = "(\n" + script + ")\n"
	if err := s.deps.Runner.WriteFile(ctx, input, []byte(script), 0600); err != nil {
		return nil, false, err
	}
	session := runner.SessionRequest{Timeout: timeout, RunID: run.ID, Phase: req.Step, Round: durable.AttemptRound(run, run.Phase), Attempt: number, PhaseDir: dir, Command: runner.ExecRequest{Executable: "/bin/bash", Args: []string{"--login", input}, Dir: gitRun.Path, Env: s.deps.Env}}
	v := &workflow.SystemStepSnapshot{OwnerID: req.OwnerID, ID: uuid.NewString(), Phase: run.Phase, Step: req.Step, Round: session.Round, Attempt: number, Commands: req.Commands, CommandTimeout: timeout.String(), TargetSHA: req.TargetSHA, WorktreeIdentity: identity, Status: "running", ProcessSession: sessions.Name(session), PhaseDir: dir}
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

// checksScript keeps all commands in one shell so variables and directory changes
// carry forward. The EXIT trap closes the output pipe before waiting for tee, and
// records explicit exits as well as set -e failures. Markers survive restarts.
func checksScript(dir string, commands []string) string {
	var script strings.Builder
	script.WriteString(`set -e
set -o pipefail
exec 3>&1 4>&2
_mergeyard_check_exit=''
_mergeyard_check_tee=''
trap '_mergeyard_check_status=$?; trap - EXIT; exec 1>&3 2>&4; if [ -n "$_mergeyard_check_tee" ]; then wait "$_mergeyard_check_tee" || :; fi; if [ -n "$_mergeyard_check_exit" ]; then printf "%s\\n" "$_mergeyard_check_status" > "$_mergeyard_check_exit"; fi; exit "$_mergeyard_check_status"' EXIT
`)
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	for i, command := range commands {
		path := filepath.Join(dir, fmt.Sprintf("command-%d", i+1))
		fmt.Fprintf(&script, "_mergeyard_check_exit=%s\n: > %s\n/usr/bin/mkfifo %s\n/usr/bin/tee %s < %s &\n_mergeyard_check_tee=$!\nexec > %s 2>&1\n", quote(path+".exit"), quote(path+".started"), quote(path+".pipe"), quote(path+".output"), quote(path+".pipe"), quote(path+".pipe"))
		script.WriteString(command + "\n")
		script.WriteString("exec 1>&3 2>&4\nwait \"$_mergeyard_check_tee\"\n_mergeyard_check_tee=''\nprintf '0\\n' > \"$_mergeyard_check_exit\"\n_mergeyard_check_exit=''\n")
	}
	return script.String()
}

func (s *Scheduler) checkResults(ctx context.Context, v *workflow.SystemStepSnapshot) ([]workflow.SystemCommandResult, error) {
	results := make([]workflow.SystemCommandResult, 0, len(v.Commands))
	for i, command := range v.Commands {
		result := workflow.SystemCommandResult{Command: command, Status: "skipped"}
		path := filepath.Join(v.PhaseDir, fmt.Sprintf("command-%d", i+1))
		if _, err := s.deps.Runner.ReadFile(ctx, path+".started"); errors.Is(err, os.ErrNotExist) {
			results = append(results, result)
			continue
		} else if err != nil {
			return nil, err
		}
		output, err := s.deps.Runner.ReadFile(ctx, path+".output")
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		result.Output = excerpt(string(output))
		result.Status = "fail"
		result.ExitCode = v.ExitCode
		exit, err := s.deps.Runner.ReadFile(ctx, path+".exit")
		if err == nil {
			code, err := strconv.Atoi(strings.TrimSpace(string(exit)))
			if err != nil {
				return nil, fmt.Errorf("invalid exit marker for check command %d: %w", i+1, err)
			}
			result.ExitCode = &code
			if code == 0 {
				result.Status = "pass"
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if result.Status == "fail" && v.TimedOut {
			result.Output = excerpt(result.Output + "\nTimed out after " + v.CommandTimeout)
		}
		results = append(results, result)
	}
	return results, nil
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

// A zero shell exit alone cannot prove that every list entry ran: exit or exec
// can bypass subsequent commands and the durable per-command exit markers.
func checkEvidenceError(v *workflow.SystemStepSnapshot) string {
	if len(v.Results) != len(v.Commands) {
		return "Configured checks lack complete command evidence"
	}
	for i, result := range v.Results {
		if result.Command != v.Commands[i] || result.Status != "pass" || result.ExitCode == nil || *result.ExitCode != 0 {
			return "Configured check lacks passing evidence: " + v.Commands[i]
		}
	}
	return ""
}

// Setup may change the workspace, while checks must test a clean pinned commit.
// The same guard serves implement, fix, handback, and cached evidence on restart.
func (s *Scheduler) verifyCheckedTree(ctx context.Context, gitRun managedgit.Run, target string) error {
	if gitRun.Branch == "" {
		g, ok := s.deps.Git.(BaseGit)
		if !ok {
			return &fault.Error{Code: "git.base_check_unsupported", Message: "Git adapter cannot inspect verification scratch"}
		}
		return g.InspectVerification(ctx, gitRun, target)
	}
	g, ok := s.deps.Git.(interface {
		InspectFixTarget(context.Context, managedgit.Run, string) error
	})
	if !ok {
		return &fault.Error{Code: "git.checks_unsupported", Message: "Git adapter cannot verify the pinned checked workspace"}
	}
	return g.InspectFixTarget(ctx, gitRun, target)
}

func runningSystemStep(run workflow.Run, step string) bool {
	for _, v := range run.SystemSteps {
		if v.Step == step && v.Phase == run.Phase && v.Round == durable.AttemptRound(run, run.Phase) && v.Status == "running" {
			return true
		}
	}
	return false
}

func storedCommandTimeout(v *workflow.SystemStepSnapshot) (time.Duration, error) {
	timeout, err := time.ParseDuration(v.CommandTimeout)
	if err != nil || timeout <= 0 {
		return 0, &fault.Error{Code: "phase.result_invalid", Message: "Stored system-step timeout is invalid", Err: err}
	}
	return timeout, nil
}
