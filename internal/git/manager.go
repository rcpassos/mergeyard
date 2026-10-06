package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/repository"
	"github.com/rcpassos/mergeyard/internal/workspace"
)

// Manager owns Git operations in a managed workspace. Callers must hold the
// workspace lock and supply persisted run ownership when preparing a run.
// Operations are serialized so base checkout and worktree metadata cannot race.
type Manager struct {
	root string
	mu   sync.Mutex
}

func New(w *workspace.Workspace) *Manager { return &Manager{root: w.Root} }

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
	SHA       string
	Committed bool
	// TreeChanged is set by CommitFix relative to its original review target.
	TreeChanged bool
}

var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
var shaPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// Prepare clones or refreshes the managed base and creates one isolated
// worktree. The base is detached so concurrent runs never check out its branch.
func (m *Manager) Prepare(ctx context.Context, req PrepareRequest) (Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	if (remoteExists || localExists || pathExists) && !owned {
		return Run{}, failure("git.branch_conflict", run.Path, errors.New("existing branch or worktree is not owned by this run"))
	}
	if pathExists {
		if err := verifyWorktree(ctx, run); err != nil {
			return Run{}, err
		}
		return run, nil
	}
	if owned {
		// A removed directory may still have registered worktree metadata.
		if _, err := command(ctx, run.BasePath, "git.worktree", "worktree", "prune", "--expire", "now"); err != nil {
			return Run{}, err
		}
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
	return run, nil
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
	return Run{
		ID: req.RunID, Repository: req.Repository, RemoteURL: remote, IssueNumber: req.IssueNumber,
		Branch:   fmt.Sprintf("mergeyard/issue-%d", req.IssueNumber),
		Path:     filepath.Join(m.root, "worktrees", slug, req.RunID),
		BasePath: filepath.Join(m.root, "repos", slug, "base"),
	}, nil
}

func (m *Manager) validateRun(run Run) error {
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
