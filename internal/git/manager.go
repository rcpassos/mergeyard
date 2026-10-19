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
	// Clone bounds the first transfer into a managed base. It is deliberately
	// larger than Fetch because there is no local history to build on.
	Clone time.Duration
	// Fetch bounds routine remote reads that build on an existing clone.
	Fetch time.Duration
	// Push bounds publication, including the repository's pre-push hook.
	Push time.Duration
}

// Defaults for Timeouts fields left unset.
const (
	defaultMetadataTimeout = 2 * time.Minute
	defaultWorktreeTimeout = 10 * time.Minute
	defaultCloneTimeout    = time.Hour
	defaultFetchTimeout    = 10 * time.Minute
	defaultPushTimeout     = 30 * time.Minute
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
	if timeouts.Clone <= 0 {
		timeouts.Clone = defaultCloneTimeout
	}
	if timeouts.Fetch <= 0 {
		timeouts.Fetch = defaultFetchTimeout
	}
	if timeouts.Push <= 0 {
		timeouts.Push = defaultPushTimeout
	}
	return &Manager{root: w.Root, timeouts: timeouts}
}

// Budget class names attached to phase contexts.
const (
	budgetMetadata = "metadata"
	budgetWorktree = "worktree"
	budgetClone    = "clone"
	budgetFetch    = "fetch"
	budgetPush     = "push"
)

// publicationBudget returns the outer budget for an operation that commits and
// publishes: every phase keeps its own class deadline instead of sharing one.
func (t Timeouts) publicationBudget() time.Duration {
	return t.Metadata + t.Worktree + t.Fetch + t.Push
}

// prepareBudget returns the outer budget for a preparation: clone, routine
// fetch, status, checkout, recovery and worktree creation each keep their own
// deadline inside it. The sum is a backstop only; every command is bounded by
// its class deadline first.
func (t Timeouts) prepareBudget() time.Duration {
	return t.Clone + 4*t.Worktree + 2*t.Fetch + 4*t.Metadata
}

// budget is attached to a phase context so an expired deadline can name the
// class budget that bounded it and how long the phase actually ran.
type budget struct {
	name    string
	limit   time.Duration
	started time.Time
}

type budgetKey struct{}

func withBudget(ctx context.Context, name string, limit time.Duration) context.Context {
	return context.WithValue(ctx, budgetKey{}, budget{name: name, limit: limit, started: time.Now()})
}

// begin serializes one managed operation, bounds it with the operation class's
// deadline, and releases a caller that has already given up. Callers must call
// the returned release function.
func (m *Manager) begin(ctx context.Context, timeout time.Duration) (context.Context, func(), error) {
	if err := m.mu.Lock(ctx); err != nil {
		return nil, nil, contextFailure(ctx, "git.wait", "", err)
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	return bounded, func() { cancel(); m.mu.Unlock() }, nil
}

// worktreeDeadline bounds the commit phase inside an operation whose outer
// budget covers every phase. Callers must call the returned cancel.
func (m *Manager) worktreeDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	return m.phaseDeadline(ctx, budgetWorktree, m.timeouts.Worktree)
}

// cloneDeadline bounds the first transfer into a managed base. Callers must
// call the returned cancel.
func (m *Manager) cloneDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	return m.phaseDeadline(ctx, budgetClone, m.timeouts.Clone)
}

// fetchDeadline bounds a routine remote read or fetch. Callers must call the
// returned cancel.
func (m *Manager) fetchDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	return m.phaseDeadline(ctx, budgetFetch, m.timeouts.Fetch)
}

// pushDeadline bounds a push and the repository's pre-push hook. Callers must
// call the returned cancel.
func (m *Manager) pushDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	return m.phaseDeadline(ctx, budgetPush, m.timeouts.Push)
}

func (m *Manager) phaseDeadline(ctx context.Context, name string, limit time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(withBudget(ctx, name, limit), limit)
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
	// RemovalIntent reports durable authorization to remove this worktree,
	// recorded by the caller after an ownership inspection. It lets cleanup
	// finish an interrupted removal whose Git link is already gone but that
	// predates the manager's own cleanup record. It is caller evidence, not
	// part of the prepared run's persisted identity.
	RemovalIntent bool `json:"-"`
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
// The first clone, routine fetch and worktree creation each keep their own
// deadline inside the outer preparation bound.
func (m *Manager) Prepare(ctx context.Context, req PrepareRequest) (Run, error) {
	ctx, release, err := m.begin(ctx, m.timeouts.prepareBudget())
	if err != nil {
		return Run{}, err
	}
	defer release()
	run, err := m.newRun(req)
	if err != nil {
		return Run{}, err
	}
	if _, err := os.Stat(run.BasePath); errors.Is(err, os.ErrNotExist) {
		if err := m.cloneBase(ctx, run); err != nil {
			return Run{}, err
		}
	} else if err != nil {
		return Run{}, failure("git.clone", run.BasePath, err)
	}
	metaCtx, metaCancel := m.phaseDeadline(ctx, budgetMetadata, m.timeouts.Metadata)
	if err := verifyBase(metaCtx, run.BasePath); err != nil {
		metaCancel()
		return Run{}, err
	}
	if _, err := verifyOrigin(metaCtx, run); err != nil {
		metaCancel()
		return Run{}, err
	}
	metaCancel()
	statusCtx, statusCancel := m.worktreeDeadline(ctx)
	status, err := command(statusCtx, run.BasePath, "git.status", "status", "--porcelain", "--untracked-files=all")
	statusCancel()
	if err != nil {
		return Run{}, err
	}
	if status != "" {
		return Run{}, failure("git.dirty_base", run.BasePath, errors.New("reconcile changes in the base checkout before starting new work"))
	}
	fetchCtx, fetchCancel := m.fetchDeadline(ctx)
	_, err = command(fetchCtx, run.BasePath, "git.fetch", "fetch", "--prune", "origin", "+refs/heads/*:refs/remotes/origin/*")
	fetchCancel()
	if err != nil {
		return Run{}, err
	}
	run.BaseBranch = req.BaseBranch
	if run.BaseBranch == "" {
		setHeadCtx, setHeadCancel := m.fetchDeadline(ctx)
		_, err = command(setHeadCtx, run.BasePath, "git.base_branch", "remote", "set-head", "origin", "--auto")
		setHeadCancel()
		if err != nil {
			return Run{}, err
		}
		refCtx, refCancel := m.phaseDeadline(ctx, budgetMetadata, m.timeouts.Metadata)
		ref, err := command(refCtx, run.BasePath, "git.base_branch", "symbolic-ref", "refs/remotes/origin/HEAD")
		refCancel()
		if err != nil {
			return Run{}, err
		}
		run.BaseBranch = strings.TrimPrefix(ref, "refs/remotes/origin/")
	}
	branchCtx, branchCancel := m.phaseDeadline(ctx, budgetMetadata, m.timeouts.Metadata)
	_, err = command(branchCtx, run.BasePath, "git.base_branch", "check-ref-format", "refs/heads/"+run.BaseBranch)
	if err == nil {
		baseRef := "refs/remotes/origin/" + run.BaseBranch
		run.BaseSHA, err = command(branchCtx, run.BasePath, "git.base_branch", "rev-parse", "--verify", baseRef+"^{commit}")
	}
	branchCancel()
	if err != nil {
		return Run{}, err
	}
	checkoutCtx, checkoutCancel := m.worktreeDeadline(ctx)
	if _, err := command(checkoutCtx, run.BasePath, "git.checkout", "checkout", "--detach", run.BaseSHA); err != nil {
		checkoutCancel()
		return Run{}, err
	}
	checkoutCancel()
	refCtx, refCancel := m.phaseDeadline(ctx, budgetMetadata, m.timeouts.Metadata)
	remoteExists, err := refExists(refCtx, run.BasePath, "refs/remotes/origin/"+run.Branch)
	if err != nil {
		refCancel()
		return Run{}, err
	}
	localExists, err := refExists(refCtx, run.BasePath, "refs/heads/"+run.Branch)
	refCancel()
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
	recoveryCtx, recoveryCancel := m.worktreeDeadline(ctx)
	if pathExists {
		if !owned {
			// Creation stopped partway, so the tree never reached an agent.
			if err := removeInterruptedWorktree(recoveryCtx, run, intent.BaseSHA); err != nil {
				recoveryCancel()
				return Run{}, err
			}
			localExists = false
		} else {
			if err := verifyWorktree(recoveryCtx, run); err != nil {
				recoveryCancel()
				return Run{}, err
			}
			recoveryCancel()
			return run, nil
		}
	} else if owned {
		// A removed directory may still have registered worktree metadata.
		if _, err := command(recoveryCtx, run.BasePath, "git.worktree", "worktree", "prune", "--expire", "now"); err != nil {
			recoveryCancel()
			return Run{}, err
		}
	}
	if intentOwned && localExists {
		// A killed 'worktree add -b' may create the branch before its directory.
		if err := removeInterruptedWorktree(recoveryCtx, run, intent.BaseSHA); err != nil {
			recoveryCancel()
			return Run{}, err
		}
		localExists = false
	}
	recoveryCancel()
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
	worktreeCtx, worktreeCancel := m.worktreeDeadline(ctx)
	if _, err := command(worktreeCtx, run.BasePath, "git.worktree", args...); err != nil {
		worktreeCancel()
		return Run{}, err
	}
	worktreeCancel()
	// A completed creation is durable once its caller persists the run, so the
	// intent lasts only while worktree add can still be interrupted.
	return run, removePrepareIntent(run)
}

// cloneBase performs the first transfer beside the managed base and renames it
// into place only after Git reports success. A killed or timed-out clone
// therefore never looks like a usable base, and the next attempt clones again
// under the clone budget instead of starting the full transfer as a routine
// fetch under the shorter fetch budget.
func (m *Manager) cloneBase(ctx context.Context, run Run) error {
	parent := filepath.Dir(run.BasePath)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return failure("git.clone", run.BasePath, err)
	}
	staging := filepath.Join(parent, ".base-clone")
	if err := os.RemoveAll(staging); err != nil {
		return failure("git.clone", run.BasePath, err)
	}
	cloneCtx, cloneCancel := m.cloneDeadline(ctx)
	_, err := command(cloneCtx, m.root, "git.clone", "clone", "--", run.RemoteURL, staging)
	cloneCancel()
	if err != nil {
		_ = os.RemoveAll(staging)
		return err
	}
	if err := os.Rename(staging, run.BasePath); err != nil {
		_ = os.RemoveAll(staging)
		return failure("git.clone", run.BasePath, fmt.Errorf("publish completed clone: %w", err))
	}
	return nil
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
	if err := writeAtomicJSON(prepareIntentPath(run), newPrepareIntent(run)); err != nil {
		return failure("git.worktree", run.Path, err)
	}
	return nil
}

// writeAtomicJSON replaces path with value in one rename so an interrupted
// write never leaves a partial intent behind.
func writeAtomicJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
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
// run, the branch must still sit at the interrupted start, and an existing
// checkout must still be the run branch attached to the managed base.
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
	ours, err := interruptedWorktreeOurs(ctx, run)
	if err != nil {
		return err
	}
	if !ours {
		return failure("git.branch_conflict", run.Path, errors.New("interrupted worktree was changed; preserved for inspection"))
	}
	if err := os.RemoveAll(run.Path); err != nil {
		return failure("git.worktree", run.Path, err)
	}
	// A killed 'worktree add' can leave an initialization lock, and prune
	// preserves locked metadata, which would keep the branch checked out.
	// Unlocking an unregistered or unlocked tree fails harmlessly; prune and
	// branch deletion below report any real problem.
	_, _ = command(ctx, run.BasePath, "git.worktree", "worktree", "unlock", "--", run.Path)
	if _, err := command(ctx, run.BasePath, "git.worktree", "worktree", "prune", "--expire", "now"); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	_, err = command(ctx, run.BasePath, "git.worktree", "branch", "-D", "--", run.Branch)
	return err
}

// interruptedWorktreeOurs reports whether a leftover path is safe to discard:
// it is empty, or it is still the run branch attached to the managed base.
// Anything whose ownership cannot be established is preserved.
func interruptedWorktreeOurs(ctx context.Context, run Run) (bool, error) {
	if _, err := os.Lstat(run.Path); errors.Is(err, os.ErrNotExist) {
		return true, nil
	} else if err != nil {
		return false, failure("git.worktree", run.Path, err)
	}
	if _, err := os.Lstat(filepath.Join(run.Path, ".git")); errors.Is(err, os.ErrNotExist) {
		// Only an empty directory is provably unused before a checkout exists.
		entries, err := os.ReadDir(run.Path)
		if err != nil {
			return false, failure("git.worktree", run.Path, err)
		}
		return len(entries) == 0, nil
	} else if err != nil {
		return false, failure("git.worktree", run.Path, err)
	}
	branch, err := command(ctx, run.Path, "git.worktree", "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		if ctx.Err() != nil {
			return false, contextFailure(ctx, "git.worktree", run.Path, ctx.Err())
		}
		// Git writes the linked registration before HEAD, so a deadline here
		// leaves no readable checkout. Accept it only when the link, backlink,
		// and common directory point at this run's managed base.
		return managedInitialization(run)
	}
	if branch != run.Branch {
		return false, nil
	}
	common, err := command(ctx, run.Path, "git.worktree", "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		if ctx.Err() != nil {
			return false, contextFailure(ctx, "git.worktree", run.Path, ctx.Err())
		}
		return false, nil
	}
	got, err := os.Stat(common)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, failure("git.worktree", run.Path, err)
	}
	want, err := os.Stat(filepath.Join(run.BasePath, ".git"))
	if err != nil {
		return false, failure("git.worktree", run.BasePath, err)
	}
	return os.SameFile(got, want), nil
}

// managedInitialization reports whether a checkout without HEAD is positively
// linked to this run's managed base and still in Git's initialization: the
// worktree link, reciprocal registration, and common directory resolve to the
// base, the initialization lock is held, and the pending HEAD names the run
// branch. Missing, foreign, or human metadata is preserved.
func managedInitialization(run Run) (bool, error) {
	link := filepath.Join(run.Path, ".git")
	info, err := os.Lstat(link)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, failure("git.worktree", run.Path, err)
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	data, err := os.ReadFile(link)
	if err != nil {
		return false, failure("git.worktree", run.Path, err)
	}
	admin, ok := gitDirTarget(string(data), run.Path)
	if !ok {
		return false, nil
	}
	root := filepath.Join(run.BasePath, ".git", "worktrees")
	rootInfo, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, failure("git.worktree", run.BasePath, err)
	}
	parent, err := os.Stat(filepath.Dir(admin))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, failure("git.worktree", run.Path, err)
	}
	adminInfo, err := os.Stat(admin)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, failure("git.worktree", run.Path, err)
	}
	if !adminInfo.IsDir() || !os.SameFile(parent, rootInfo) {
		return false, nil
	}
	back, err := os.ReadFile(filepath.Join(admin, "gitdir"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, failure("git.worktree", run.Path, err)
	}
	backPath := strings.TrimSpace(string(back))
	if !filepath.IsAbs(backPath) {
		backPath = filepath.Join(admin, backPath)
	}
	backInfo, err := os.Stat(backPath)
	if err != nil {
		return false, nil
	}
	if !os.SameFile(backInfo, info) {
		return false, nil
	}
	common, err := os.ReadFile(filepath.Join(admin, "commondir"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, failure("git.worktree", run.Path, err)
	}
	commonPath := strings.TrimSpace(string(common))
	if !filepath.IsAbs(commonPath) {
		commonPath = filepath.Join(admin, commonPath)
	}
	got, err := os.Stat(commonPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, failure("git.worktree", run.Path, err)
	}
	want, err := os.Stat(filepath.Join(run.BasePath, ".git"))
	if err != nil {
		return false, failure("git.worktree", run.BasePath, err)
	}
	if !os.SameFile(got, want) {
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(admin, "HEAD")); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, failure("git.worktree", run.Path, err)
	}
	// Only Git's own initialization state may stand in for a missing HEAD: the
	// worktree is still locked for initialization and the pending HEAD names
	// exactly the run branch. Human edits that removed HEAD lack both.
	locked, err := os.ReadFile(filepath.Join(admin, "locked"))
	if err != nil {
		return false, nil
	}
	if strings.TrimSpace(string(locked)) != "initializing" {
		return false, nil
	}
	pending, err := os.ReadFile(filepath.Join(admin, "HEAD.lock"))
	if err != nil {
		return false, nil
	}
	if strings.TrimSpace(string(pending)) != "ref: refs/heads/"+run.Branch {
		return false, nil
	}
	return true, nil
}

// gitDirTarget parses a linked worktree's ".git" file and resolves a relative
// target against the worktree directory.
func gitDirTarget(content, worktree string) (string, bool) {
	target, ok := strings.CutPrefix(strings.TrimSpace(content), "gitdir:")
	if !ok {
		return "", false
	}
	target = strings.TrimSpace(target)
	if target == "" || strings.ContainsAny(target, "\x00\r\n") {
		return "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(worktree, target)
	}
	return filepath.Clean(target), true
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
