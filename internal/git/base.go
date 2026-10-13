package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// BaseMerge is produced without touching the run's index, files, or branch.
type BaseMerge struct {
	BaseBranch string
	BaseSHA    string
	HeadSHA    string
	TreeSHA    string
	Conflicts  []string
}

// CheckBase fetches the current base before computing a real three-way merge.
// Exit 1 is a merge conflict; all other errors leave mergeability unknown.
func (m *Manager) CheckBase(ctx context.Context, run Run, branch, head string) (BaseMerge, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := BaseMerge{BaseBranch: branch, HeadSHA: head}
	if err := m.validateRun(run); err != nil {
		return v, err
	}
	if !shaPattern.MatchString(head) {
		return v, failure("git.base_check", run.Path, errors.New("invalid approved head"))
	}
	if err := verifyBase(ctx, run.BasePath); err != nil {
		return v, err
	}
	if _, err := verifyOrigin(ctx, run); err != nil {
		return v, err
	}
	if _, err := command(ctx, run.BasePath, "git.base_check", "check-ref-format", "refs/heads/"+branch); err != nil {
		return v, err
	}
	if _, err := command(ctx, run.BasePath, "git.fetch", "fetch", "--no-tags", "origin", "+refs/heads/"+branch+":refs/remotes/origin/"+branch); err != nil {
		return v, err
	}
	var err error
	v.BaseSHA, err = command(ctx, run.BasePath, "git.base_check", "rev-parse", "--verify", "refs/remotes/origin/"+branch+"^{commit}")
	if err != nil {
		return v, err
	}
	out, err := command(ctx, run.BasePath, "git.base_check", "merge-tree", "--write-tree", "--name-only", "-z", v.BaseSHA, head)
	var exit *exec.ExitError
	if err != nil && !(ctx.Err() == nil && errors.As(err, &exit) && exit.ExitCode() == 1) {
		return v, err
	}
	fields := strings.Split(out, "\x00")
	if len(fields) == 0 || !shaPattern.MatchString(fields[0]) {
		return v, failure("git.base_check", run.BasePath, errors.New("invalid merge-tree output"))
	}
	v.TreeSHA = fields[0]
	if err != nil {
		for _, path := range fields[1:] {
			if path == "" {
				break
			}
			v.Conflicts = append(v.Conflicts, path)
		}
		if len(v.Conflicts) == 0 {
			return v, failure("git.base_check", run.BasePath, errors.New("merge conflict lacks paths"))
		}
	}
	return v, nil
}

// VerificationCommit creates an unreferenced synthetic merge commit for checkout.
// It is never put on a branch or pushed.
func (m *Manager) VerificationCommit(ctx context.Context, run Run, v BaseMerge) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validateRun(run); err != nil {
		return "", err
	}
	if !shaPattern.MatchString(v.TreeSHA) || !shaPattern.MatchString(v.BaseSHA) || !shaPattern.MatchString(v.HeadSHA) || len(v.Conflicts) != 0 {
		return "", failure("git.base_check", run.BasePath, errors.New("invalid clean merge"))
	}
	return command(ctx, run.BasePath, "git.base_check", "-c", "user.name=Mergeyard", "-c", "user.email=mergeyard@example.invalid", "commit-tree", v.TreeSHA, "-p", v.BaseSHA, "-p", v.HeadSHA, "-m", "Mergeyard current-base verification")
}

// VerificationRun derives a disposable path from persisted run and check IDs.
// The caller journals this ownership before EnsureVerification creates it.
func (m *Manager) VerificationRun(run Run, id, commit string) (Run, error) {
	if err := m.validateRun(run); err != nil {
		return Run{}, err
	}
	if !runIDPattern.MatchString(id) || !shaPattern.MatchString(commit) {
		return Run{}, failure("git.base_check", run.Path, errors.New("invalid verification ownership"))
	}
	run.Path = filepath.Join(filepath.Dir(run.Path), run.ID+"-verify-"+id)
	run.Branch = ""
	return run, nil
}

func (m *Manager) EnsureVerification(ctx context.Context, run Run, id, commit string) (Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	scratch, err := m.VerificationRun(run, id, commit)
	if err != nil {
		return Run{}, err
	}
	if _, err := os.Lstat(scratch.Path); err == nil {
		return scratch, inspectVerification(ctx, scratch, commit, false)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Run{}, err
	}
	// Complete interrupted removal before recreating the same journaled tree.
	if _, err := command(ctx, run.BasePath, "git.base_check", "worktree", "prune", "--expire", "now"); err != nil {
		return Run{}, err
	}
	_, err = command(ctx, run.BasePath, "git.base_check", "worktree", "add", "--detach", "--", scratch.Path, commit)
	return scratch, err
}

func inspectVerification(ctx context.Context, scratch Run, commit string, clean bool) error {
	info, err := os.Lstat(scratch.Path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return failure("git.base_check", scratch.Path, errors.New("scratch path is not a directory"))
	}
	common, err := command(ctx, scratch.Path, "git.base_check", "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	actual, err := os.Stat(common)
	if err != nil {
		return err
	}
	expected, err := os.Stat(filepath.Join(scratch.BasePath, ".git"))
	if err != nil {
		return err
	}
	if !os.SameFile(actual, expected) {
		return failure("git.base_check", scratch.Path, errors.New("scratch is not attached to the managed base"))
	}
	branch, err := command(ctx, scratch.Path, "git.base_check", "symbolic-ref", "--quiet", "HEAD")
	var exit *exec.ExitError
	if branch != "" || !(errors.As(err, &exit) && exit.ExitCode() == 1) {
		return failure("git.base_check", scratch.Path, errors.New("scratch is not detached"))
	}
	if clean {
		return inspectFixTarget(ctx, scratch, commit)
	}
	return nil
}

func (m *Manager) InspectVerification(ctx context.Context, scratch Run, target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Recover original ownership from the deterministic path before inspection.
	original := scratch
	original.Path = filepath.Join(filepath.Dir(scratch.Path), scratch.ID)
	expected, err := m.newRun(PrepareRequest{Repository: scratch.Repository, RemoteURL: scratch.RemoteURL, RunID: scratch.ID, IssueNumber: scratch.IssueNumber})
	if err != nil {
		return err
	}
	original.Branch = expected.Branch
	if err := m.validateRun(original); err != nil {
		return err
	}
	prefix := scratch.ID + "-verify-"
	id, ok := strings.CutPrefix(filepath.Base(scratch.Path), prefix)
	if !ok {
		return failure("git.base_check", scratch.Path, errors.New("invalid scratch path"))
	}
	planned, err := m.VerificationRun(original, id, target)
	if err != nil {
		return err
	}
	if planned.Path != scratch.Path {
		return failure("git.base_check", scratch.Path, errors.New("invalid scratch path"))
	}
	return inspectVerification(ctx, scratch, target, true)
}

// RemoveVerification only deletes an owned detached scratch. Its files are
// disposable, including generated and ignored output; run work is never removed.
// The caller must stop and verify the owned command processes first.
func (m *Manager) RemoveVerification(ctx context.Context, run Run, id, commit string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	scratch, err := m.VerificationRun(run, id, commit)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(scratch.Path); err == nil {
		if err := inspectVerification(ctx, scratch, commit, false); err != nil {
			return err
		}
		if _, err := command(ctx, run.BasePath, "git.base_cleanup", "worktree", "remove", "--force", "--", scratch.Path); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err = command(ctx, run.BasePath, "git.base_cleanup", "worktree", "prune", "--expire", "now")
	return err
}

// PrepareBaseRepair starts the pinned merge outside the agent sandbox. The
// caller persists creation intent first. A replay keeps resolutions and stages
// intact when the same MERGE_HEAD is already present.
func (m *Manager) PrepareBaseRepair(ctx context.Context, run Run, target, base string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.validateRun(run); err != nil {
		return err
	}
	if !shaPattern.MatchString(target) || !shaPattern.MatchString(base) {
		return failure("git.base_repair", run.Path, errors.New("invalid pinned merge commits"))
	}
	if err := verifyWorktree(ctx, run); err != nil {
		return err
	}
	if _, err := verifyOrigin(ctx, run); err != nil {
		return err
	}
	pending, err := command(ctx, run.Path, "git.base_repair", "rev-parse", "--verify", "--quiet", "MERGE_HEAD")
	var exit *exec.ExitError
	if err != nil && !(ctx.Err() == nil && errors.As(err, &exit) && exit.ExitCode() == 1) {
		return err
	}
	if err == nil {
		head, err := command(ctx, run.Path, "git.base_repair", "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		if pending != base || head != target {
			return failure("git.base_repair", run.Path, errors.New("pending merge differs from the journaled base and head"))
		}
		return nil
	}
	if err := inspectFixTarget(ctx, run, target); err != nil {
		return err
	}
	_, err = command(ctx, run.Path, "git.base_repair", "merge", "--no-commit", "--no-ff", "--no-edit", "--no-autostash", base)
	if err != nil && !(ctx.Err() == nil && errors.As(err, &exit) && exit.ExitCode() == 1) {
		return err
	}
	pending, checkErr := command(ctx, run.Path, "git.base_repair", "rev-parse", "--verify", "--quiet", "MERGE_HEAD")
	if checkErr != nil {
		return checkErr
	}
	if pending != base {
		return failure("git.base_repair", run.Path, errors.New("merge did not establish the journaled base parent"))
	}
	return nil
}
