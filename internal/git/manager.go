package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/gate"
	"github.com/rcpassos/mergeyard/internal/pathspec"
	"github.com/rcpassos/mergeyard/internal/repository"
	"github.com/rcpassos/mergeyard/internal/workspace"
)

// Manager owns Git operations in a managed workspace. Callers must hold the
// workspace lock and supply persisted run ownership when preparing a run.
// Operations are serialized so base checkout and worktree metadata cannot race.
type Manager struct {
	root     string
	timeouts Timeouts
	// mu serializes operations through a cancellation-aware gate so a caller
	// that gives up is released instead of waiting for the operation in flight.
	mu gate.Gate
}

// Timeouts bound each managed operation class so one stalled remote, hook, or
// repository read cannot hold the scheduler's tick indefinitely. A caller that
// supplies its own deadline keeps the earlier one.
type Timeouts struct {
	// Metadata bounds quick local reads of refs, the index, or single paths.
	Metadata time.Duration
	// Worktree bounds operations that may run repository hooks, stage or
	// restore the worktree, or create and remove one.
	Worktree time.Duration
	// Remote bounds operations that contact the configured remote.
	Remote time.Duration
}

// Defaults for Timeouts fields left unset.
const (
	defaultMetadataTimeout = 2 * time.Minute
	defaultWorktreeTimeout = 10 * time.Minute
	defaultRemoteTimeout   = 10 * time.Minute
)

func New(w *workspace.Workspace) *Manager { return NewWithTimeouts(w, Timeouts{}) }

// NewWithTimeouts selects operation deadlines; tests use shorter ones.
func NewWithTimeouts(w *workspace.Workspace, timeouts Timeouts) *Manager {
	if timeouts.Metadata <= 0 {
		timeouts.Metadata = defaultMetadataTimeout
	}
	if timeouts.Worktree <= 0 {
		timeouts.Worktree = defaultWorktreeTimeout
	}
	if timeouts.Remote <= 0 {
		timeouts.Remote = defaultRemoteTimeout
	}
	return &Manager{root: w.Root, timeouts: timeouts}
}

// publication returns the budget for an operation that commits and publishes:
// every phase keeps its own class deadline instead of sharing one.
func (t Timeouts) publication() time.Duration { return t.Worktree + t.Remote }

// begin serializes one managed operation, bounds it with the operation class's
// deadline, and releases a caller that has already given up. Callers must call
// the returned release function.
func (m *Manager) begin(ctx context.Context, timeout time.Duration) (context.Context, func(), error) {
	if err := m.mu.Lock(ctx); err != nil {
		return nil, nil, contextFailure("git.wait", "", err)
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	return bounded, func() { cancel(); m.mu.Unlock() }, nil
}

// worktreeDeadline bounds the commit phase inside an operation whose outer
// budget covers every phase. Callers must call the returned cancel.
func (m *Manager) worktreeDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, m.timeouts.Worktree)
}

// remoteDeadline bounds the remote phase inside an operation whose outer budget
// covers every phase. Callers must call the returned cancel.
func (m *Manager) remoteDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, m.timeouts.Remote)
}

// PrepareRequest describes a new or reconcilable run. RemoteURL defaults to
// https://github.com/<Repository>.git; an override supports local mirrors.
type PrepareRequest struct {
	Repository  string
	RemoteURL   string
	BaseBranch  string
	RunID       string
	IssueNumber int
	KnownRuns   []Run
}

// Run holds the paths and refs required to resume Git operations. Persist this
// value with the run: BaseSHA pins the fetched base used for no-diff detection.
type Run struct {
	Exclude     []string
	ID          string
	Repository  string
	RemoteURL   string
	IssueNumber int
	Branch      string
	Path        string
	BasePath    string
	BaseBranch  string
	BaseSHA     string
	// PublishedSHA is optional durable evidence from the observed merged PR head.
	PublishedSHA string
}

// Phase describes commit metadata and whether a diff is required. Implement
// requires a diff; a fix that disputes every finding can proceed without one.
type Phase struct {
	Title          string
	RequireChanges bool
}

type CommitResult struct {
	Diagnostics []Diagnostic
	SHA         string
	Committed   bool
	// TreeChanged is set by CommitFix relative to its original review target.
	TreeChanged bool
}

type Diagnostic struct {
	Code    string `json:"code"`
	Path    string `json:"path"`
	Message string `json:"message"`
}

var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
var shaPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// Prepare clones or refreshes the managed base and creates one isolated
// worktree. The base is detached so concurrent runs never check out its branch.
func (m *Manager) Prepare(ctx context.Context, req PrepareRequest) (Run, error) {
	ctx, release, err := m.begin(ctx, m.timeouts.Remote)
	if err != nil {
		return Run{}, err
	}
	defer release()
	run, err := m.newRun(req)
	if err != nil {
		return Run{}, err
	}
	if _, err := os.Stat(run.BasePath); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(run.BasePath), 0700); err != nil {
			return Run{}, failure("git.clone", run.BasePath, err)
		}
		if _, err := command(ctx, m.root, "git.clone", "clone", "--", run.RemoteURL, run.BasePath); err != nil {
			return Run{}, err
		}
	} else if err != nil {
		return Run{}, failure("git.clone", run.BasePath, err)
	}
	if err := verifyBase(ctx, run.BasePath); err != nil {
		return Run{}, err
	}
	if _, err := verifyOrigin(ctx, run); err != nil {
		return Run{}, err
	}
	status, err := command(ctx, run.BasePath, "git.status", "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return Run{}, err
	}
	if status != "" {
		return Run{}, failure("git.dirty_base", run.BasePath, errors.New("reconcile changes in the base checkout before starting new work"))
	}
	if _, err := command(ctx, run.BasePath, "git.fetch", "fetch", "--prune", "origin", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return Run{}, err
	}
	run.BaseBranch = req.BaseBranch
	if run.BaseBranch == "" {
		if _, err := command(ctx, run.BasePath, "git.base_branch", "remote", "set-head", "origin", "--auto"); err != nil {
			return Run{}, err
		}
		ref, err := command(ctx, run.BasePath, "git.base_branch", "symbolic-ref", "refs/remotes/origin/HEAD")
		if err != nil {
			return Run{}, err
		}
		run.BaseBranch = strings.TrimPrefix(ref, "refs/remotes/origin/")
	}
	if _, err := command(ctx, run.BasePath, "git.base_branch", "check-ref-format", "refs/heads/"+run.BaseBranch); err != nil {
		return Run{}, err
	}
	baseRef := "refs/remotes/origin/" + run.BaseBranch
	run.BaseSHA, err = command(ctx, run.BasePath, "git.base_branch", "rev-parse", "--verify", baseRef+"^{commit}")
	if err != nil {
		return Run{}, err
	}
	if _, err := command(ctx, run.BasePath, "git.checkout", "checkout", "--detach", run.BaseSHA); err != nil {
		return Run{}, err
	}
	remoteExists, err := refExists(ctx, run.BasePath, "refs/remotes/origin/"+run.Branch)
	if err != nil {
		return Run{}, err
	}
	localExists, err := refExists(ctx, run.BasePath, "refs/heads/"+run.Branch)
	if err != nil {
		return Run{}, err
	}
	owned := false
	for _, known := range req.KnownRuns {
		if known.ID == run.ID && known.Repository == run.Repository && known.IssueNumber == run.IssueNumber &&
			known.Branch == run.Branch && known.Path == run.Path && known.BasePath == run.BasePath &&
			known.RemoteURL == run.RemoteURL && shaPattern.MatchString(known.BaseSHA) {
			owned = true
			run.BaseSHA, run.BaseBranch = known.BaseSHA, known.BaseBranch
			break
		}
	}
	_, pathErr := os.Lstat(run.Path)
	pathExists := pathErr == nil
	if pathErr != nil && !errors.Is(pathErr, os.ErrNotExist) {
		return Run{}, failure("git.worktree", run.Path, pathErr)
	}
	// A prepared intent proves this run created its deterministic branch and
	// path before a deadline could interrupt worktree creation. A remote branch
	// cannot come from worktree creation, so only persisted ownership covers it.
	var intent prepareIntent
	intentOwned := false
	if !owned {
		var err error
		intent, intentOwned, err = loadPrepareIntent(run)
		if err != nil {
			return Run{}, err
		}
	}
	if remoteExists && !owned {
		return Run{}, failure("git.branch_conflict", run.Path, errors.New("existing branch or worktree is not owned by this run"))
	}
	if (localExists || pathExists) && !owned && !intentOwned {
		return Run{}, failure("git.branch_conflict", run.Path, errors.New("existing branch or worktree is not owned by this run"))
	}
	if pathExists {
		if !owned {
			// Creation stopped partway, so the tree never reached an agent.
			if err := removeInterruptedWorktree(ctx, run, intent.BaseSHA); err != nil {
				return Run{}, err
			}
			localExists = false
		} else {
			if err := verifyWorktree(ctx, run); err != nil {
				return Run{}, err
			}
			return run, nil
		}
	} else if owned {
		// A removed directory may still have registered worktree metadata.
		if _, err := command(ctx, run.BasePath, "git.worktree", "worktree", "prune", "--expire", "now"); err != nil {
			return Run{}, err
		}
	}
	if intentOwned && localExists {
		// A killed 'worktree add -b' may create the branch before its directory.
		if err := removeInterruptedWorktree(ctx, run, intent.BaseSHA); err != nil {
			return Run{}, err
		}
		localExists = false
	}
	if err := writePrepareIntent(run); err != nil {
		return Run{}, err
	}
	if err := os.MkdirAll(filepath.Dir(run.Path), 0700); err != nil {
		return Run{}, failure("git.worktree", run.Path, err)
	}
	args := []string{"worktree", "add"}
	if localExists {
		args = append(args, run.Path, run.Branch)
	} else {
		start := run.BaseSHA
		if remoteExists {
			start = "refs/remotes/origin/" + run.Branch
		}
		args = append(args, "-b", run.Branch, run.Path, start)
	}
	if _, err := command(ctx, run.BasePath, "git.worktree", args...); err != nil {
		return Run{}, err
	}
	// A completed creation is durable once its caller persists the run, so the
	// intent lasts only while worktree add can still be interrupted.
	return run, removePrepareIntent(run)
}

func (m *Manager) newRun(req PrepareRequest) (Run, error) {
	if !repository.ValidName(req.Repository) || !runIDPattern.MatchString(req.RunID) || req.IssueNumber <= 0 {
		return Run{}, failure("git.invalid_input", "", errors.New("expected owner/repo, a safe run ID, and a positive issue number"))
	}
	remote := req.RemoteURL
	if remote == "" {
		remote = "https://github.com/" + req.Repository + ".git"
	}
	if strings.HasPrefix(remote, "-") || strings.ContainsAny(remote, "\x00\r\n") {
		return Run{}, failure("git.invalid_input", "", errors.New("invalid remote URL"))
	}
	slug := strings.ReplaceAll(req.Repository, "/", "-")
	path, err := WorktreePath(m.root, req.Repository, req.RunID)
	if err != nil {
		return Run{}, err
	}
	return Run{
		ID: req.RunID, Repository: req.Repository, RemoteURL: remote, IssueNumber: req.IssueNumber,
		Branch:   fmt.Sprintf("mergeyard/issue-%d", req.IssueNumber),
		Path:     path,
		BasePath: filepath.Join(m.root, "repos", slug, "base"),
	}, nil
}

// WorktreePath returns the managed worktree directory for one run. The runtime
// prepares exactly this path; the CLI rebuilds it from the run's repository and
// ID so a reply from the port can never choose where an interactive harness
// runs.
func WorktreePath(root, repo, runID string) (string, error) {
	if !repository.ValidName(repo) || !runIDPattern.MatchString(runID) {
		return "", failure("git.invalid_input", "", errors.New("expected owner/repo and a safe run ID"))
	}
	return filepath.Join(root, "worktrees", strings.ReplaceAll(repo, "/", "-"), runID), nil
}

// prepareIntent is a claim written before branch and worktree creation and
// removed once creation succeeds. If the deadline interrupts 'worktree add',
// the next attempt recognizes the deterministic branch and path as its own
// leftovers. BaseSHA is the interrupted branch's start, so a branch that moved
// past it carries work and is preserved instead of deleted.
type prepareIntent struct {
	ID          string `json:"id"`
	Repository  string `json:"repository"`
	RemoteURL   string `json:"remote_url"`
	IssueNumber int    `json:"issue_number"`
	Branch      string `json:"branch"`
	Path        string `json:"path"`
	BasePath    string `json:"base_path"`
	BaseSHA     string `json:"base_sha"`
}

func prepareIntentPath(run Run) string {
	return filepath.Join(filepath.Dir(run.Path), run.ID+".intent")
}

func newPrepareIntent(run Run) prepareIntent {
	return prepareIntent{
		ID: run.ID, Repository: run.Repository, RemoteURL: run.RemoteURL,
		IssueNumber: run.IssueNumber, Branch: run.Branch, Path: run.Path,
		BasePath: run.BasePath, BaseSHA: run.BaseSHA,
	}
}

// writePrepareIntent atomically replaces the intent so a crash mid-write
// never leaves a partial claim behind.
func writePrepareIntent(run Run) error {
	fail := func(err error) error { return failure("git.worktree", run.Path, err) }
	data, err := json.Marshal(newPrepareIntent(run))
	if err != nil {
		return fail(err)
	}
	path := prepareIntentPath(run)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fail(err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+run.ID+".intent-*")
	if err != nil {
		return fail(err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return fail(err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fail(err)
	}
	return nil
}

func loadPrepareIntent(run Run) (prepareIntent, bool, error) {
	data, err := os.ReadFile(prepareIntentPath(run))
	if errors.Is(err, os.ErrNotExist) {
		return prepareIntent{}, false, nil
	}
	if err != nil {
		return prepareIntent{}, false, failure("git.worktree", run.Path, err)
	}
	var intent prepareIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		return prepareIntent{}, false, failure("git.worktree", run.Path, fmt.Errorf("invalid prepare intent: %w", err))
	}
	want := newPrepareIntent(run)
	// The fetched base may have advanced since the interrupted attempt, so the
	// identity match ignores BaseSHA; the recorded start still guards cleanup.
	identity := intent
	identity.BaseSHA, want.BaseSHA = "", ""
	return intent, identity == want, nil
}

func removePrepareIntent(run Run) error {
	if err := os.Remove(prepareIntentPath(run)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return failure("git.worktree", run.Path, err)
	}
	return nil
}

// removeInterruptedWorktree discards a partial creation that never reached an
// agent. The intent proves the deterministic path and branch belong to this
// run, and the branch must still sit at the interrupted start.
func removeInterruptedWorktree(ctx context.Context, run Run, start string) error {
	exists, err := refExists(ctx, run.BasePath, "refs/heads/"+run.Branch)
	if err != nil {
		return err
	}
	if exists {
		head, err := command(ctx, run.BasePath, "git.worktree", "rev-parse", "--verify", "refs/heads/"+run.Branch)
		if err != nil {
			return err
		}
		if head != start {
			return failure("git.branch_conflict", run.Path, errors.New("interrupted worktree creation left commits; preserved for inspection"))
		}
	}
	if err := os.RemoveAll(run.Path); err != nil {
		return failure("git.worktree", run.Path, err)
	}
	if _, err := command(ctx, run.BasePath, "git.worktree", "worktree", "prune", "--expire", "now"); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	_, err = command(ctx, run.BasePath, "git.worktree", "branch", "-D", "--", run.Branch)
	return err
}

func (m *Manager) validateRun(run Run) error {
	for _, pattern := range run.Exclude {
		if err := pathspec.Validate(pattern); err != nil {
			return failure("git.invalid_input", run.Path, err)
		}
	}
	expected, err := m.newRun(PrepareRequest{Repository: run.Repository, RemoteURL: run.RemoteURL, RunID: run.ID, IssueNumber: run.IssueNumber})
	if err != nil {
		return failure("git.invalid_input", run.Path, err)
	}
	if run.Branch != expected.Branch || run.Path != expected.Path || run.BasePath != expected.BasePath ||
		run.RemoteURL != expected.RemoteURL || !shaPattern.MatchString(run.BaseSHA) {
		return failure("git.invalid_input", run.Path, errors.New("run does not match the managed workspace layout"))
	}
	return nil
}

func verifyWorktree(ctx context.Context, run Run) error {
	branch, err := command(ctx, run.Path, "git.worktree_mismatch", "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return err
	}
	if branch != run.Branch {
		return failure("git.worktree_mismatch", run.Path, errors.New("worktree is not on the run branch"))
	}
	for _, check := range []struct{ option, expected string }{
		{"--show-toplevel", run.Path},
		{"--git-common-dir", filepath.Join(run.BasePath, ".git")},
	} {
		actual, err := command(ctx, run.Path, "git.worktree_mismatch", "rev-parse", "--path-format=absolute", check.option)
		if err != nil {
			return err
		}
		got, err := os.Stat(actual)
		if err != nil {
			return failure("git.worktree_mismatch", run.Path, err)
		}
		want, err := os.Stat(check.expected)
		if err != nil {
			return failure("git.worktree_mismatch", run.Path, err)
		}
		if !os.SameFile(got, want) {
			return failure("git.worktree_mismatch", run.Path, errors.New("worktree is not attached to the managed base checkout"))
		}
	}
	return nil
}

func verifyBase(ctx context.Context, path string) error {
	base, err := os.Lstat(path)
	if err != nil {
		return failure("git.base_checkout", path, err)
	}
	metadata, err := os.Lstat(filepath.Join(path, ".git"))
	if err != nil {
		return failure("git.base_checkout", path, err)
	}
	if !base.IsDir() || !metadata.IsDir() {
		return failure("git.base_checkout", path, errors.New("base must be a managed checkout with its own Git directory"))
	}
	root, err := command(ctx, path, "git.base_checkout", "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	actual, err := os.Stat(root)
	if err != nil {
		return failure("git.base_checkout", path, err)
	}
	if !os.SameFile(base, actual) {
		return failure("git.base_checkout", path, errors.New("base path is not the repository root"))
	}
	return nil
}

func failure(code, path string, err error) *fault.Error {
	return &fault.Error{Code: code, Path: path, Err: err}
}
