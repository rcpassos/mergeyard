package testkit

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/escalation"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/sessions"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

// scenario keeps the real scheduler and durable store. External work completes
// only through the runner's queued outcomes; no shell, Git or tmux is invoked.
// The real adapters still build commands and parse native result artifacts.
type Scenario struct {
	T       *testing.T
	Runtime *app.Runtime
	Cfg     config.Config
	Api     *FakeGitHub
	Git     *ScenarioGit
	Runner  *ScenarioRunner
}

func NewScenario(t *testing.T) *Scenario {
	t.Helper()
	rt, err := app.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &Scenario{T: t, Runtime: rt}
	t.Cleanup(func() { f.Runtime.Close() })
	f.Cfg, _, err = config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
	if err != nil {
		t.Fatal(err)
	}
	f.Cfg.Agents.Claude.Executable = filepath.Join(rt.Workspace.Root, "fake-claude")
	f.Cfg.Agents.Codex.Executable = filepath.Join(rt.Workspace.Root, "fake-codex")
	f.Git = &ScenarioGit{T: t, Root: rt.Workspace.Root, Runs: map[string]managedgit.Run{}, Heads: map[string]string{},
		LocalHeads: map[string]string{}, Commits: map[string]map[string]managedgit.ReviewFile{}, Sequence: 1}
	f.Api = &FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {Ready(7)}}, Head: func(branch string) string { return f.Git.Heads[branch] }}
	f.Runner = &ScenarioRunner{Local: runner.NewLocal(runner.Options{}), T: t, Sessions: map[string]*ScenarioSession{}, Outcomes: map[string][]ScenarioOutcome{}}
	return f
}

func (f *Scenario) New(cfg config.Config, deps scheduler.Dependencies) (*scheduler.Scheduler, error) {
	if deps.Git == nil {
		deps.Git = f.Git
	}
	if deps.Runner == nil {
		deps.Runner = f.Runner
	}
	if deps.GitHub == nil {
		deps.GitHub = f.Api
	}
	return TestScheduler(cfg, SchedulerResources(f.Runtime), deps)
}

func (f *Scenario) Reopen() {
	f.T.Helper()
	root := f.Runtime.Workspace.Root
	if err := f.Runtime.Close(); err != nil {
		f.T.Fatal(err)
	}
	rt, err := app.Open(f.T.Context(), root)
	if err != nil {
		f.T.Fatal(err)
	}
	f.Runtime = rt
}

// until advances a bounded number of real ticks rather than polling wall time.
func (f *Scenario) Until(s *scheduler.Scheduler, state workflow.State, phase workflow.Phase) workflow.Run {
	f.T.Helper()
	var runs []workflow.Run
	for range 100 {
		if err := s.Tick(f.T.Context()); err != nil {
			f.T.Fatal(err)
		}
		var err error
		runs, err = s.Runs(f.T.Context())
		if err != nil {
			f.T.Fatal(err)
		}
		if len(runs) == 1 && runs[0].State == state && runs[0].Phase == phase {
			return runs[0]
		}
		if len(runs) == 1 && runs[0].State == workflow.NeedsAttention && state != workflow.NeedsAttention {
			f.T.Fatalf("unexpected attention: %s: %s", runs[0].LastErrorCode, runs[0].LastErrorMessage)
		}
	}
	f.T.Fatalf("100 ticks did not reach %s/%s: %+v", state, phase, runs)
	return workflow.Run{}
}

type ScenarioOutcome struct {
	Stdout, Stderr []byte
	Exit           int
	Hold           bool
	Apply          func(runner.SessionRequest)
	Identity       string
	StderrFor      func(runner.SessionRequest) []byte
}

type ScenarioSession struct {
	Request runner.SessionRequest
	Outcome ScenarioOutcome
	Status  runner.SessionStatus
}

type ScenarioRunner struct {
	*runner.Local
	T           *testing.T
	Sessions    map[string]*ScenarioSession
	Outcomes    map[string][]ScenarioOutcome
	Requests    []runner.SessionRequest
	BeforeStart func(runner.SessionRequest)
	AfterStart  func(runner.SessionRequest)
}

// nativeOutcome extracts the existing literal native event fixtures. It never
// interprets shell code: only the JSON between the first '{' and last '}'.
func NativeOutcome(event string) ScenarioOutcome {
	start, end := strings.IndexByte(event, '{'), strings.LastIndexByte(event, '}')
	if start < 0 || end < start || !json.Valid([]byte(event[start:end+1])) {
		panic("expected one literal native JSON fixture")
	}
	return ScenarioOutcome{Stdout: []byte(event[start:end+1] + "\n")}
}

func (r *ScenarioRunner) Queue(phase string, outcomes ...ScenarioOutcome) {
	r.Outcomes[phase] = append(r.Outcomes[phase], outcomes...)
}
func (r *ScenarioRunner) Exec(context.Context, runner.ExecRequest) (runner.ExecResult, error) {
	r.T.Fatal("scenario unexpectedly executed a system command")
	return runner.ExecResult{}, nil
}
func (r *ScenarioRunner) StartSession(ctx context.Context, req runner.SessionRequest) (runner.SessionRef, error) {
	if err := ctx.Err(); err != nil {
		return runner.SessionRef{}, err
	}
	if r.BeforeStart != nil {
		r.BeforeStart(req)
	}
	ref := runner.SessionRef{Name: sessions.Name(req), PhaseDir: req.PhaseDir}
	if r.Sessions[ref.Name] != nil {
		return runner.SessionRef{}, fmt.Errorf("duplicate scenario launch: %s", ref.Name)
	}
	outcome := NativeOutcome(SuccessfulScript)
	if req.Phase == "review" {
		outcome = NativeOutcome(ApprovedReview)
	}
	if req.Phase == "fix" {
		outcome = NativeOutcome(CiFixedReport)
		outcome.Apply = ScenarioWrite(r.T, "feature.txt", "fixed")
	}
	if queued := r.Outcomes[req.Phase]; len(queued) > 0 {
		outcome = queued[0]
		r.Outcomes[req.Phase] = queued[1:]
	}
	r.Requests = append(r.Requests, req)
	r.Sessions[ref.Name] = &ScenarioSession{Request: req, Outcome: outcome, Status: runner.SessionStatus{State: runner.SessionRunning}}
	if r.AfterStart != nil {
		r.AfterStart(req)
	}
	return ref, nil
}
func (r *ScenarioRunner) Complete(ref runner.SessionRef) {
	session := r.Sessions[ref.Name]
	if session == nil {
		r.T.Fatalf("unknown scenario session %s", ref.Name)
	}
	req, outcome := session.Request, session.Outcome
	if outcome.Apply != nil {
		outcome.Apply(req)
	} else if req.Phase == "implement" && outcome.Exit == 0 {
		if err := os.WriteFile(filepath.Join(req.Command.Dir, "feature.txt"), []byte("implemented\n"), 0600); err != nil {
			r.T.Fatal(err)
		}
	}
	if outcome.StderrFor != nil {
		outcome.Stderr = outcome.StderrFor(req)
	}
	stdout := outcome.Stdout
	if strings.Contains(filepath.Base(req.Command.Executable), "codex") {
		identity := CodexID
		if req.Phase == "review" {
			identity = ReviewerCodexID
		}
		if outcome.Identity != "" {
			identity = outcome.Identity
		}
		stdout = []byte(fmt.Sprintf("{\"type\":\"thread.started\",\"thread_id\":%q}\n{\"type\":\"turn.completed\"}\n", identity))
		if len(outcome.Stdout) > 0 {
			var event struct {
				Output json.RawMessage `json:"structured_output"`
			}
			if err := json.Unmarshal(outcome.Stdout, &event); err != nil {
				r.T.Fatal(err)
			}
			for i, arg := range req.Command.Args {
				if arg == "-o" && i+1 < len(req.Command.Args) {
					if err := os.WriteFile(req.Command.Args[i+1], event.Output, 0600); err != nil {
						r.T.Fatal(err)
					}
				}
			}
		}
	}
	exit := outcome.Exit
	session.Status = runner.SessionStatus{State: runner.SessionExited, ExitCode: &exit}
	metadata, _ := json.Marshal(map[string]int{"exit_code": exit})
	for name, data := range map[string][]byte{"events.jsonl": stdout, "stderr.log": outcome.Stderr, "exit.json": metadata} {
		if err := os.WriteFile(filepath.Join(ref.PhaseDir, name), data, 0600); err != nil {
			r.T.Fatal(err)
		}
	}
}
func (r *ScenarioRunner) SessionStatus(ctx context.Context, ref runner.SessionRef) (runner.SessionStatus, error) {
	if err := ctx.Err(); err != nil {
		return runner.SessionStatus{}, err
	}
	session := r.Sessions[ref.Name]
	if session == nil {
		return runner.SessionStatus{State: runner.SessionMissing}, nil
	}
	if session.Status.State == runner.SessionRunning && !session.Outcome.Hold {
		r.Complete(ref)
	}
	return session.Status, nil
}
func (r *ScenarioRunner) StopSession(ctx context.Context, ref runner.SessionRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if session := r.Sessions[ref.Name]; session != nil && session.Status.State == runner.SessionRunning {
		exit := 130
		session.Status = runner.SessionStatus{State: runner.SessionExited, ExitCode: &exit}
	}
	return nil
}
func (r *ScenarioRunner) ListSessions(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var names []string
	for name, s := range r.Sessions {
		if s.Status.State == runner.SessionRunning {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// scenarioGit records externally observed refs and snapshots. It does not
// emulate Git commands or recompute merge/ownership policy from production.
type ScenarioGit struct {
	T          *testing.T
	Root       string
	Runs       map[string]managedgit.Run
	Heads      map[string]string
	LocalHeads map[string]string
	After      func(string)
	Commits    map[string]map[string]managedgit.ReviewFile
	Sequence   int
}

func (g *ScenarioGit) Prepare(ctx context.Context, req managedgit.PrepareRequest) (managedgit.Run, error) {
	if err := ctx.Err(); err != nil {
		return managedgit.Run{}, err
	}
	if run, ok := g.Runs[req.RunID]; ok {
		return run, nil
	}
	branch := fmt.Sprintf("mergeyard/issue-%d", req.IssueNumber)
	path := filepath.Join(g.Root, "worktrees", strings.ReplaceAll(req.Repository, "/", "-"), req.RunID)
	if err := os.MkdirAll(path, 0700); err != nil {
		return managedgit.Run{}, err
	}
	if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("initial\n"), 0600); err != nil {
		return managedgit.Run{}, err
	}
	run := managedgit.Run{ID: req.RunID, Repository: req.Repository, IssueNumber: req.IssueNumber, Branch: branch, Path: path, BasePath: filepath.Join(g.Root, "repos", strings.ReplaceAll(req.Repository, "/", "-"), "base"), BaseBranch: "main", BaseSHA: fmt.Sprintf("%040x", 1)}
	g.Runs[run.ID] = run
	g.Heads[branch] = run.BaseSHA
	g.LocalHeads[branch] = run.BaseSHA
	g.Commits[run.BaseSHA] = g.Files(path)
	return run, nil
}
func (g *ScenarioGit) Files(path string) map[string]managedgit.ReviewFile {
	g.T.Helper()
	files := map[string]managedgit.ReviewFile{}
	err := filepath.WalkDir(path, func(pathname string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(pathname)
		if err != nil {
			return err
		}
		name, err := filepath.Rel(path, pathname)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		files[name] = managedgit.ReviewFile{Data: data, Mode: info.Mode()}
		return nil
	})
	if err != nil {
		g.T.Fatal(err)
	}
	return files
}
func (g *ScenarioGit) ConfirmOwnership(ctx context.Context, _ managedgit.Run) error { return ctx.Err() }
func (g *ScenarioGit) Inspect(ctx context.Context, _ managedgit.Run) error          { return ctx.Err() }
func (g *ScenarioGit) ListWorktrees(ctx context.Context) ([]string, error) {
	var paths []string
	for _, run := range g.Runs {
		paths = append(paths, run.Path)
	}
	sort.Strings(paths)
	return paths, ctx.Err()
}
func (g *ScenarioGit) DiscardPreparation(ctx context.Context, _ managedgit.PrepareRequest) (managedgit.Leftovers, error) {
	return managedgit.Leftovers{}, ctx.Err()
}
func (g *ScenarioGit) Commit(ctx context.Context, run managedgit.Run) (managedgit.CommitResult, error) {
	if err := ctx.Err(); err != nil {
		return managedgit.CommitResult{}, err
	}
	files := g.Files(run.Path)
	head := g.LocalHeads[run.Branch]
	changed := !reflect.DeepEqual(files, g.Commits[head])
	if changed {
		g.Sequence++
		head = fmt.Sprintf("%040x", g.Sequence)
		g.LocalHeads[run.Branch] = head
		g.Commits[head] = files
	}
	return managedgit.CommitResult{SHA: head, Committed: changed, TreeChanged: changed}, nil
}
func (g *ScenarioGit) CommitAndPush(ctx context.Context, run managedgit.Run, _ managedgit.Phase) (managedgit.CommitResult, error) {
	result, err := g.Commit(ctx, run)
	if err == nil {
		g.Heads[run.Branch] = result.SHA
	}
	return result, err
}
func (g *ScenarioGit) SnapshotReview(ctx context.Context, run managedgit.Run) (managedgit.ReviewSnapshot, error) {
	return managedgit.ReviewSnapshot{RunID: run.ID, Head: g.Heads[run.Branch], Files: g.Files(run.Path)}, ctx.Err()
}
func (g *ScenarioGit) ReviewDiff(ctx context.Context, _ managedgit.Run, _ string) (string, error) {
	return "diff --git a/feature.txt b/feature.txt\n+implemented\n", ctx.Err()
}
func (g *ScenarioGit) ReviewChanged(ctx context.Context, run managedgit.Run, before managedgit.ReviewSnapshot) (bool, error) {
	return g.Heads[run.Branch] != before.Head || !reflect.DeepEqual(g.Files(run.Path), before.Files), ctx.Err()
}
func (g *ScenarioGit) RestoreReview(ctx context.Context, run managedgit.Run, before managedgit.ReviewSnapshot) error {
	if g.After != nil {
		g.After("before-restoration")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.RemoveAll(run.Path); err != nil {
		return err
	}
	if err := os.MkdirAll(run.Path, 0700); err != nil {
		return err
	}
	for name, file := range before.Files {
		path := filepath.Join(run.Path, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(path, file.Data, file.Mode); err != nil {
			return err
		}
	}
	g.LocalHeads[run.Branch] = before.Head
	if g.After != nil {
		g.After("restoration")
	}
	return nil
}
func (g *ScenarioGit) InspectFixTarget(ctx context.Context, _ managedgit.Run, _ string) error {
	return ctx.Err()
}
func (g *ScenarioGit) CommitFix(ctx context.Context, run managedgit.Run, _ managedgit.Phase, target string) (managedgit.CommitResult, error) {
	result, err := g.Commit(ctx, run)
	if err == nil {
		result.TreeChanged = !reflect.DeepEqual(g.Commits[result.SHA], g.Commits[target])
		if g.After != nil {
			g.After("commit")
		}
	}
	return result, err
}
func (g *ScenarioGit) PushFix(ctx context.Context, run managedgit.Run, _, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	g.Heads[run.Branch] = target
	if g.After != nil {
		g.After("push")
	}
	return nil
}
func (g *ScenarioGit) InspectRetry(ctx context.Context, run managedgit.Run, _ string, _ bool) (string, error) {
	return g.Heads[run.Branch], ctx.Err()
}
func (g *ScenarioGit) EscalationDiff(ctx context.Context, _ managedgit.Run, _ string) ([]escalation.FileChange, error) {
	return nil, ctx.Err()
}
func (g *ScenarioGit) CheckBase(ctx context.Context, run managedgit.Run, branch, head string) (managedgit.BaseMerge, error) {
	return managedgit.BaseMerge{BaseBranch: branch, BaseSHA: run.BaseSHA, HeadSHA: head, TreeSHA: head}, ctx.Err()
}
func (g *ScenarioGit) VerificationCommit(context.Context, managedgit.Run, managedgit.BaseMerge) (string, error) {
	panic("scenario did not configure base verification")
}
func (g *ScenarioGit) VerificationRun(managedgit.Run, string, string) (managedgit.Run, error) {
	panic("scenario did not configure base verification")
}
func (g *ScenarioGit) EnsureVerification(context.Context, managedgit.Run, string, string) (managedgit.Run, error) {
	panic("scenario did not configure base verification")
}
func (g *ScenarioGit) InspectVerification(context.Context, managedgit.Run, string) error {
	panic("scenario did not configure base verification")
}
func (g *ScenarioGit) RemoveVerification(context.Context, managedgit.Run, string, string) error {
	panic("scenario did not configure base verification")
}

var _ runner.Runner = (*ScenarioRunner)(nil)
var _ scheduler.Git = (*ScenarioGit)(nil)
var _ scheduler.ReviewGit = (*ScenarioGit)(nil)
var _ scheduler.FixGit = (*ScenarioGit)(nil)
var _ scheduler.BaseGit = (*ScenarioGit)(nil)

func (f *Scenario) Parts() (*scheduler.Scheduler, *app.Runtime, *FakeGitHub, string, config.Config, *ScenarioRunner) {
	f.T.Helper()
	s, err := f.New(f.Cfg, scheduler.Dependencies{GitHub: BranchGitHub{f.Api}})
	if err != nil {
		f.T.Fatal(err)
	}
	return s, f.Runtime, f.Api, "", f.Cfg, f.Runner
}
func (g *ScenarioGit) CleanupWorktree(ctx context.Context, run managedgit.Run) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.RemoveAll(run.Path); err != nil {
		return err
	}
	delete(g.Runs, run.ID)
	return nil
}
func (g *ScenarioGit) CleanupBranch(ctx context.Context, run managedgit.Run) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	delete(g.Heads, run.Branch)
	return nil
}

func NewPairingScenario(t *testing.T, implementer, reviewer, fix string) *Scenario {
	t.Helper()
	f := NewScenario(t)
	f.Cfg.Repositories[0].Implementer = config.Role{Agent: implementer, Model: "implement-model", Effort: "medium", Skills: []string{"implement"}, MaxAttempts: 1}
	f.Cfg.Repositories[0].Reviewer = config.Role{Agent: reviewer, Model: "review-model", Effort: "low", Skills: []string{"review"}, MaxAttempts: 1}
	approved := ApprovedAfterDisputesReview
	outcome := NativeOutcome(fix)
	if strings.Contains(fix, `"resolution":"fixed"`) {
		approved = ApprovedAfterFixReview
		outcome.Apply = ScenarioWrite(t, "feature.txt", "fixed")
	}
	f.Runner.Queue("review", NativeOutcome(ChangesReview), NativeOutcome(approved))
	f.Runner.Queue("fix", outcome)
	return f
}
func ScenarioWrite(t *testing.T, name, value string) func(runner.SessionRequest) {
	return func(req runner.SessionRequest) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(req.Command.Dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *Scenario) Advance(s *scheduler.Scheduler, done func(workflow.Run) bool) workflow.Run {
	f.T.Helper()
	var runs []workflow.Run
	for range 100 {
		if err := s.Tick(f.T.Context()); err != nil {
			f.T.Fatal(err)
		}
		var err error
		runs, err = s.Runs(f.T.Context())
		if err != nil {
			f.T.Fatal(err)
		}
		if len(runs) == 1 && done(runs[0]) {
			return runs[0]
		}
	}
	f.T.Fatalf("scenario predicate was not reached: %+v", runs)
	return workflow.Run{}
}
