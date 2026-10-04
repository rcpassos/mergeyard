package doctor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/rcpassos/mergeyard/internal/config"
)

func (c *checker) api(endpoint string, paginate bool) ([]byte, error) {
	args := []string{"api", "--hostname", "github.com", "--method", "GET"}
	if paginate {
		args = append(args, "--paginate")
	}
	result, err := c.command("", "gh", append(args, endpoint)...)
	return result.Stdout, err
}

func (c *checker) repository(repo config.Repository) {
	pushAllowed := c.pushPermission(repo.Repo)
	c.labels(repo)
	files := c.repositoryFiles(repo, pushAllowed)
	c.skills(repo.Implementer, repo.Repo+" implementer", files)
	c.skills(repo.Reviewer, repo.Repo+" reviewer", files)
	if files == nil {
		c.report.add(Unverifiable, "harness.repository_files_unverifiable", repo.Repo, "cannot inspect instruction files and repository skills without a fetched base branch")
		return
	}
	for _, role := range []config.Role{repo.Implementer, repo.Reviewer} {
		if role.Agent == "codex" && files["CLAUDE.md"] && !files["AGENTS.md"] {
			c.report.add(Warning, "harness.instructions_missing", repo.Repo, "Codex is assigned but the base branch has CLAUDE.md without AGENTS.md; add AGENTS.md for repository instructions")
			break
		}
	}
	if v, known := c.versions["claude"]; known && v.less(claudeMinimum) && files["AGENTS.md"] && !files["CLAUDE.md"] && (repo.Implementer.Agent == "claude" || repo.Reviewer.Agent == "claude") {
		c.report.add(Warning, "harness.instructions_missing", repo.Repo, "Claude below 2.1.277 cannot read AGENTS.md without CLAUDE.md; upgrade Claude or add CLAUDE.md")
	}
}

func (c *checker) pushPermission(repo string) bool {
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
		Permissions *struct {
			Push *bool `json:"push"`
		} `json:"permissions"`
	}
	if json.Unmarshal(data, &metadata) != nil || metadata == nil {
		c.report.add(Error, "github.invalid_response", repo, "expected repository metadata from GitHub")
		return true
	}
	if metadata.Permissions == nil || metadata.Permissions.Push == nil {
		c.report.add(Unverifiable, "github.push_unverifiable", repo, "GitHub did not report permissions.push for the authenticated account")
		return true
	}
	if !*metadata.Permissions.Push {
		c.report.add(Error, "github.push_forbidden", repo, "the authenticated GitHub account does not have push permission")
		return false
	}
	return true
}

func (c *checker) labels(repo config.Repository) {
	if !c.tools["gh"] {
		c.report.add(Unverifiable, "github.labels_unverifiable", repo.Repo, "labels cannot be checked without gh")
		return
	}
	data, err := c.api("repos/"+repo.Repo+"/labels?per_page=100", true)
	if err != nil {
		c.report.add(Error, "github.labels_check_failed", repo.Repo, "cannot list repository labels: "+err.Error())
		return
	}
	names, err := labelNames(data)
	if err != nil {
		c.report.add(Error, "github.invalid_response", repo.Repo, err.Error())
		return
	}
	for _, name := range []string{repo.Labels.Ready, repo.Labels.Running, repo.Labels.NeedsAttention} {
		if !names[strings.ToLower(name)] {
			c.report.add(Error, "github.label_missing", repo.Repo, fmt.Sprintf("required label %q is missing; create it or update this repository's label configuration", name))
		}
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
func (c *checker) repositoryFiles(repo config.Repository, pushAllowed bool) map[string]bool {
	if !c.tools["git"] {
		c.report.add(Unverifiable, "git.access_unverifiable", repo.Repo, "origin, base branch, fetch, and push transport cannot be checked without git")
		return nil
	}
	remote := "https://github.com/" + repo.Repo + ".git"
	refs, err := c.command("", "git", "ls-remote", "--symref", "--", remote, "HEAD")
	if err != nil {
		c.report.add(Error, "git.origin_inaccessible", repo.Repo, "cannot access origin with the machine's Git credentials: "+err.Error())
		return nil
	}
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
		c.report.add(Error, "git.base_branch", repo.Repo, fmt.Sprintf("base branch %q cannot be resolved on origin", branch))
		return nil
	}
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
	if _, err := c.command(dir, "git", "fetch", "--depth=1", "--no-tags", "--no-recurse-submodules", "--", remote, ref+":refs/heads/doctor"); err != nil {
		c.report.add(Error, "git.fetch", repo.Repo, "fetch failed: "+err.Error())
		return nil
	}
	if pushAllowed {
		// --dry-run does not contact receive hooks or prove branch-protection
		// acceptance. GitHub permissions above establish account authorization.
		target := "refs/heads/mergeyard/" + filepath.Base(dir)
		if _, err := c.command(dir, "git", "-c", "push.gpgSign=false", "push", "--dry-run", "--no-verify", "--no-force", "--no-follow-tags", "--recurse-submodules=no", "--", remote, "refs/heads/doctor:"+target); err != nil {
			c.report.add(Error, "git.push_access", repo.Repo, "push transport check failed (dry run): "+err.Error())
		}
	}
	tree, err := c.command(dir, "git", "ls-tree", "-r", "--name-only", "refs/heads/doctor")
	if err != nil {
		c.report.add(Unverifiable, "git.tree_unverifiable", repo.Repo, "cannot inspect fetched base branch: "+err.Error())
		return nil
	}
	files := make(map[string]bool)
	for _, file := range strings.Split(string(tree.Stdout), "\n") {
		files[file] = true
	}
	return files
}
