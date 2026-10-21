package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/harness/registry"
	"github.com/rcpassos/mergeyard/internal/runner"
)

// accessSeverity keeps a check unverifiable when GitHub could not be reached,
// including a command that ran past the doctor's deadline, so a transient
// outage does not stop start from serving; failures GitHub answered stay
// errors.
func accessSeverity(stderr []byte, err error) Severity {
	if githubUnreachable(stderr, err) {
		return Unverifiable
	}
	return Error
}

// githubUnreachable reports whether a failed GitHub command could not be
// reached or never answered before the deadline.
func githubUnreachable(stderr []byte, err error) bool {
	return github.Unavailable(stderr) || errors.Is(err, context.DeadlineExceeded)
}

// probeUnreachable probes the API once, because gh reports a connection
// failure during an authentication check as an invalid token.
func (c *checker) probeUnreachable() bool {
	result, err := c.api("rate_limit", false)
	return err != nil && githubUnreachable(result.Stderr, err)
}

func (c *checker) api(endpoint string, paginate bool) (runner.ExecResult, error) {
	args := []string{"api", "--hostname", "github.com", "--method", "GET"}
	if paginate {
		args = append(args, "--paginate")
	}
	return c.command("", "gh", append(args, endpoint)...)
}

func (c *checker) repository(repo config.Repository) {
	pushAllowed := c.pushPermission(repo)
	c.labels(repo)
	files := c.repositoryFiles(repo, pushAllowed)
	c.skills(repo.Implementer, repo.Repo+" implementer", files)
	c.skills(repo.Reviewer, repo.Repo+" reviewer", files)
	if files == nil {
		c.report.add(Unverifiable, "harness.repository_files_unverifiable", repo.Repo, "cannot inspect instruction files and repository skills without a fetched base branch")
		return
	}
	instructionsMatch := true
	if files["CLAUDE.md"] == fileUnverifiable || files["AGENTS.md"] == fileUnverifiable {
		instructionsMatch = false
		c.report.add(Unverifiable, "harness.instructions_unverifiable", repo.Repo, "instruction-file symlink targets leave the repository and cannot be verified for the future worktree")
	}
	for _, role := range []config.Role{repo.Implementer, repo.Reviewer} {
		if role.Agent == registry.Codex && files["CLAUDE.md"] == fileRegular && files["AGENTS.md"] != fileRegular && files["AGENTS.md"] != fileUnverifiable {
			instructionsMatch = false
			c.report.add(Warning, "harness.instructions_missing", repo.Repo, "Codex is assigned but the base branch has CLAUDE.md without AGENTS.md; add AGENTS.md for repository instructions")
			break
		}
	}
	claudeAssigned := repo.Implementer.Agent == registry.Claude || repo.Reviewer.Agent == registry.Claude
	agentsWithoutClaude := files["AGENTS.md"] == fileRegular && files["CLAUDE.md"] != fileRegular && files["CLAUDE.md"] != fileUnverifiable
	if claudeAssigned && agentsWithoutClaude {
		v, known := c.versions[registry.Claude]
		switch {
		case !known:
			instructionsMatch = false
			c.report.add(Unverifiable, "harness.instructions_unverifiable", repo.Repo, "cannot verify whether AGENTS.md alone provides instructions without a parseable Claude version")
		case v.less(claudeMinimum):
			instructionsMatch = false
			c.report.add(Warning, "harness.instructions_missing", repo.Repo, "Claude below 2.1.277 cannot read AGENTS.md without CLAUDE.md; upgrade Claude or add CLAUDE.md")
		}
	}
	if instructionsMatch {
		c.report.pass("harness.instructions_match", repo.Repo, "instruction files match the assigned harnesses")
	}
}

func (c *checker) pushPermission(repository config.Repository) bool {
	repo := repository.Repo
	if !c.tools["gh"] {
		c.report.add(Unverifiable, "github.push_unverifiable", repo, "GitHub push permission cannot be checked without gh")
		return true // Still probe Git transport when possible.
	}
	data, err := c.api("repos/"+repo, false)
	if err != nil {
		c.report.add(Unverifiable, "github.push_unverifiable", repo, "cannot read GitHub repository permissions: "+err.Error())
		return true
	}
	var metadata *struct {
		Private     *bool `json:"private"`
		Permissions *struct {
			Push *bool `json:"push"`
		} `json:"permissions"`
	}
	if json.Unmarshal(data.Stdout, &metadata) != nil || metadata == nil {
		c.report.add(Error, "github.invalid_response", repo, "expected repository metadata from GitHub")
		return true
	}
	if metadata.Private != nil && !*metadata.Private {
		if len(repository.TrustedLabelers) == 0 {
			c.report.add(Warning, "github.public_without_trusted_labelers", repo, "public repository has no trusted_labelers; write, maintain, and admin permission can authorize local agent execution. Configure an explicit list to narrow trust")
		} else {
			c.report.pass("github.trusted_labelers", repo, "public repository narrows trust to the configured trusted labelers")
		}
	}
	if metadata.Permissions == nil || metadata.Permissions.Push == nil {
		c.report.add(Unverifiable, "github.push_unverifiable", repo, "GitHub did not report permissions.push for the authenticated account")
		return true
	}
	if !*metadata.Permissions.Push {
		c.report.add(Error, "github.push_forbidden", repo, "the authenticated GitHub account does not have push permission")
		return false
	}
	c.report.pass("github.push_permission", repo, "the authenticated GitHub account has push permission")
	return true
}

func (c *checker) labels(repo config.Repository) {
	if !c.tools["gh"] {
		c.report.add(Unverifiable, "github.labels_unverifiable", repo.Repo, "labels cannot be checked without gh")
		return
	}
	data, err := c.api("repos/"+repo.Repo+"/labels?per_page=100", true)
	if err != nil {
		c.report.add(accessSeverity(data.Stderr, err), "github.labels_check_failed", repo.Repo, "cannot list repository labels: "+err.Error())
		return
	}
	names, err := labelNames(data.Stdout)
	if err != nil {
		c.report.add(Error, "github.invalid_response", repo.Repo, err.Error())
		return
	}
	missing := false
	for _, name := range []string{repo.Labels.Ready, repo.Labels.Running, repo.Labels.NeedsAttention} {
		if !names[strings.ToLower(name)] {
			missing = true
			c.report.add(Error, "github.label_missing", repo.Repo, fmt.Sprintf("required label %q is missing; create it or update this repository's label configuration", name))
		}
	}
	if !missing {
		c.report.pass("github.labels_ready", repo.Repo, "required labels exist")
	}
}

func labelNames(data []byte) (map[string]bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	names := make(map[string]bool)
	pages := 0
	for {
		var labels []struct {
			Name string `json:"name"`
		}
		err := decoder.Decode(&labels)
		if err == io.EOF {
			break
		}
		if err != nil || labels == nil {
			return nil, fmt.Errorf("expected label arrays from GitHub")
		}
		pages++
		for _, label := range labels {
			if label.Name == "" {
				return nil, fmt.Errorf("GitHub returned a label without a name")
			}
			names[strings.ToLower(label.Name)] = true
		}
	}
	if pages == 0 {
		return nil, fmt.Errorf("GitHub returned no label array")
	}
	return names, nil
}

// Fetch into a disposable bare repository. No managed clone, index, worktree,
// branch, or FETCH_HEAD is touched, and no checkout hooks can run.
func (c *checker) repositoryFiles(repo config.Repository, pushAllowed bool) map[string]repositoryFileType {
	if !c.tools["git"] {
		c.report.add(Unverifiable, "git.access_unverifiable", repo.Repo, "origin, base branch, fetch, and push transport cannot be checked without git")
		return nil
	}
	remote := "https://github.com/" + repo.Repo + ".git"
	refs, err := c.command("", "git", "ls-remote", "--symref", "--", remote, "HEAD")
	if err != nil {
		if githubUnreachable(refs.Stderr, err) {
			c.report.add(Unverifiable, "git.origin_inaccessible", repo.Repo, "origin is unreachable while GitHub is unavailable: "+err.Error())
		} else {
			c.report.add(Error, "git.origin_inaccessible", repo.Repo, "cannot access origin with the machine's Git credentials: "+err.Error())
		}
		return nil
	}
	c.report.pass("git.origin_accessible", repo.Repo, "origin is accessible")
	branch := repo.BaseBranch
	if branch == "" {
		for _, line := range strings.Split(string(refs.Stdout), "\n") {
			if strings.HasPrefix(line, "ref: refs/heads/") && strings.HasSuffix(line, "\tHEAD") {
				branch = strings.TrimSuffix(strings.TrimPrefix(line, "ref: refs/heads/"), "\tHEAD")
				break
			}
		}
	}
	if branch == "" {
		c.report.add(Error, "git.base_branch", repo.Repo, "origin has no resolvable default branch; configure base_branch")
		return nil
	}
	ref := "refs/heads/" + branch
	if _, err := c.command("", "git", "check-ref-format", ref); err != nil {
		c.report.add(Error, "git.base_branch", repo.Repo, "invalid base branch: "+err.Error())
		return nil
	}
	result, err := c.command("", "git", "ls-remote", "--exit-code", "--heads", "--", remote, ref)
	if err != nil || !strings.Contains(string(result.Stdout), "\t"+ref+"\n") {
		severity := Error
		// --exit-code reports 2 for a branch origin does not have; any other
		// failure may be an unreachable GitHub.
		if err != nil && result.ExitCode != 2 {
			severity = accessSeverity(result.Stderr, err)
		}
		c.report.add(severity, "git.base_branch", repo.Repo, fmt.Sprintf("base branch %q cannot be resolved on origin", branch))
		return nil
	}
	c.report.pass("git.base_branch", repo.Repo, fmt.Sprintf("base branch %q resolves on origin", branch))
	dir, err := os.MkdirTemp("", "mergeyard-doctor-")
	if err != nil {
		c.report.add(Error, "workspace.probe_failed", repo.Repo, err.Error())
		return nil
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			c.report.add(Error, "workspace.probe_cleanup", repo.Repo, err.Error())
		}
	}()
	// GitHub repositories use SHA1. An unrelated init.defaultObjectFormat
	// or GIT_DEFAULT_HASH setting must not make this access probe fail.
	if _, err := c.command(dir, "git", "init", "--bare", "--template=", "--object-format=sha1"); err != nil {
		c.report.add(Error, "git.fetch", repo.Repo, "cannot initialize temporary fetch probe: "+err.Error())
		return nil
	}
	if fetch, err := c.command(dir, "git", "fetch", "--depth=1", "--no-tags", "--no-recurse-submodules", "--", remote, ref+":refs/heads/doctor"); err != nil {
		c.report.add(accessSeverity(fetch.Stderr, err), "git.fetch", repo.Repo, "fetch failed: "+err.Error())
		return nil
	}
	c.report.pass("git.fetch", repo.Repo, "shallow fetch succeeded")
	if pushAllowed {
		// --dry-run does not contact receive hooks or prove branch-protection
		// acceptance. GitHub permissions above establish account authorization.
		target := "refs/heads/mergeyard/" + filepath.Base(dir)
		if push, err := c.command(dir, "git", "-c", "push.gpgSign=false", "push", "--dry-run", "--no-verify", "--no-force", "--no-follow-tags", "--recurse-submodules=no", "--", remote, "refs/heads/doctor:"+target); err != nil {
			c.report.add(accessSeverity(push.Stderr, err), "git.push_access", repo.Repo, "push transport check failed (dry run): "+err.Error())
		} else {
			c.report.pass("git.push_access", repo.Repo, "dry-run push transport succeeded")
		}
	}
	paths := repositoryFilePaths(repo)
	inputPath := filepath.Join(dir, "doctor-file-checks")
	var input strings.Builder
	for _, path := range paths {
		fmt.Fprintf(&input, "refs/heads/doctor:%s\n", path)
	}
	if err := os.WriteFile(inputPath, []byte(input.String()), 0600); err != nil {
		c.report.add(Unverifiable, "git.tree_unverifiable", repo.Repo, "cannot prepare repository file checks: "+err.Error())
		return nil
	}
	// Git follows the committed entry modes, including symlinked ancestors,
	// and returns the target's object type. A symlink blob alone never counts
	// as an available regular file; dangling links and loops are explicit.
	tree, err := c.commandInput(dir, inputPath, "git", "cat-file", "--batch-check=%(objecttype)", "--follow-symlinks")
	if err != nil {
		c.report.add(Unverifiable, "git.tree_unverifiable", repo.Repo, "cannot inspect fetched base branch: "+err.Error())
		return nil
	}
	files, err := repositoryFileTypes(tree.Stdout, paths)
	if err != nil {
		c.report.add(Unverifiable, "git.tree_unverifiable", repo.Repo, "cannot interpret repository file checks: "+err.Error())
		return nil
	}
	c.report.pass("git.tree_readable", repo.Repo, "base branch files and skills can be inspected")
	return files
}
