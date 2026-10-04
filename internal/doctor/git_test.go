package doctor_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/doctor"
	"github.com/rcpassos/mergeyard/internal/runner"
)

type localGitProbe struct {
	fake                          *fakeCommands
	local                         *runner.Local
	remote, gitConfig, hookMarker string
	directories                   []string
}

func (p *localGitProbe) Exec(ctx context.Context, req runner.ExecRequest) (runner.ExecResult, error) {
	if req.Executable != "git" {
		return p.fake.Exec(ctx, req)
	}
	if req.Dir != "" {
		p.directories = append(p.directories, req.Dir)
	}
	for i, arg := range req.Args {
		if arg == "https://github.com/octo/repo.git" {
			req.Args[i] = p.remote
		}
	}
	req.Env["GIT_CONFIG_GLOBAL"], req.Env["GIT_CONFIG_NOSYSTEM"] = p.gitConfig, "1"
	req.Env["MERGEYARD_TEST_HOOK_MARKER"] = p.hookMarker
	return p.local.Exec(ctx, req)
}

func TestRealGitProbeReportsMisconfigurationWithoutMutations(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	path, fake, opts := repositorySetup(t, "")
	dir := t.TempDir()
	work, remote := filepath.Join(dir, "work"), filepath.Join(dir, "remote.git")
	if err := os.Mkdir(work, 0700); err != nil {
		t.Fatal(err)
	}
	realGit := func(cwd string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...)
		cmd.Dir = cwd
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
		return string(output)
	}
	realGit(work, "init", "--template=", "--object-format=sha1", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "CLAUDE.md"), []byte("# Instructions\n"), 0600); err != nil {
		t.Fatal(err)
	}
	realGit(work, "add", "CLAUDE.md")
	realGit(work, "-c", "user.name=Doctor Test", "-c", "user.email=doctor@example.test", "commit", "--no-gpg-sign", "-m", "fixture")
	realGit(dir, "clone", "--bare", "--template=", work, remote)
	before := realGit(remote, "for-each-ref", "--format=%(refname) %(objectname)")
	workspacePath := filepath.Join(dir, "workspace")
	if err := os.Mkdir(workspacePath, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(workspacePath, "preserve.txt")
	if err := os.WriteFile(marker, []byte("keep\n"), 0600); err != nil {
		t.Fatal(err)
	}
	setConfig(t, path, "workspace", workspacePath)
	fake.responses["gh api --hostname github.com --method GET --paginate repos/octo/repo/labels?per_page=100"] = runner.ExecResult{Stdout: []byte(`[{"name":"ready-for-agent"}]`)}
	hooks := filepath.Join(dir, "hooks")
	if err := os.Mkdir(hooks, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"reference-transaction", "pre-push"} {
		if err := os.WriteFile(filepath.Join(hooks, name), []byte("#!/bin/sh\necho hook-ran > \"$MERGEYARD_TEST_HOOK_MARKER\"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	gitConfig, hookMarker := filepath.Join(dir, "gitconfig"), filepath.Join(dir, "hook-ran")
	if err := os.WriteFile(gitConfig, []byte("[core]\n\thooksPath = "+hooks+"\n[init]\n\tdefaultObjectFormat = sha256\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DEFAULT_HASH", "sha256")
	probe := &localGitProbe{fake: fake, local: runner.NewLocal(runner.Options{}), remote: remote, gitConfig: gitConfig, hookMarker: hookMarker}
	opts.Executor = probe
	report := doctor.Check(context.Background(), path, opts)
	requireFinding(t, report, doctor.Error, "github.label_missing", "octo/repo")
	requireFinding(t, report, doctor.Warning, "harness.instructions_missing", "octo/repo")
	for _, finding := range report.Findings {
		if strings.HasPrefix(finding.Code, "git.") {
			t.Fatalf("real Git probe failed: %+v", report)
		}
	}
	after := realGit(remote, "for-each-ref", "--format=%(refname) %(objectname)")
	if before != after {
		t.Fatalf("probe changed remote refs: before=%s after=%s", before, after)
	}
	if _, err := os.Stat(hookMarker); !os.IsNotExist(err) {
		t.Fatalf("probe ran a Git hook: %v", err)
	}
	for _, dir := range probe.directories {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("probe leaked temporary repository: %v", err)
		}
	}
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(cfg.Workspace)
	if err != nil || len(entries) != 1 || entries[0].Name() != "preserve.txt" {
		t.Fatalf("doctor changed managed workspace: %v, %v", entries, err)
	}
}

func TestRepositorySkillSymlinkAvailability(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	const skillPath = ".agents/skills/review/SKILL.md"
	for _, tc := range []struct {
		name     string
		files    []string
		links    map[string]string
		severity doctor.Severity
		code     string
	}{
		{"dangling link", nil, map[string]string{skillPath: "missing.md"}, doctor.Error, "harness.skill_missing"},
		{"valid link", []string{"shared/skill.md"}, map[string]string{skillPath: "../../../shared/skill.md"}, "", ""},
		{"valid chain", []string{"shared/skill.md"}, map[string]string{skillPath: "current.md", ".agents/skills/review/current.md": "../../../shared/skill.md"}, "", ""},
		{"linked directory", []string{"shared/SKILL.md"}, map[string]string{".agents/skills/review": "../../shared"}, "", ""},
		{"link cycle", nil, map[string]string{skillPath: "again.md", ".agents/skills/review/again.md": "SKILL.md"}, doctor.Error, "harness.skill_missing"},
		{"directory named SKILL.md", []string{skillPath + "/README.md"}, nil, doctor.Error, "harness.skill_missing"},
		{"file used as directory", []string{"file"}, map[string]string{skillPath: "../../../file/child"}, doctor.Error, "harness.skill_missing"},
		{"outside repository", nil, map[string]string{skillPath: "../../../../outside.md"}, doctor.Unverifiable, "harness.skill_unverifiable"},
		{"absolute target with newline", nil, map[string]string{skillPath: "/outside\nfile.md"}, doctor.Unverifiable, "harness.skill_unverifiable"},
		{"valid target with newline", []string{"shared/a\nb.md"}, map[string]string{skillPath: "../../../shared/a\nb.md"}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, fake, opts := repositorySetup(t, "")
			setConfig(t, path, "repositories.0.implementer.skills", []string{"review"})
			dir := t.TempDir()
			work, remote := filepath.Join(dir, "work"), filepath.Join(dir, "remote.git")
			for _, file := range append([]string{"AGENTS.md"}, tc.files...) {
				path := filepath.Join(work, file)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("# Instructions\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for name, target := range tc.links {
				path := filepath.Join(work, name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			git := func(cwd string, args ...string) {
				t.Helper()
				cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...)
				cmd.Dir = cwd
				cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, output)
				}
			}
			git(work, "init", "--template=", "--object-format=sha1", "-b", "main")
			git(work, "add", ".")
			git(work, "-c", "user.name=Doctor Test", "-c", "user.email=doctor@example.test", "commit", "--no-gpg-sign", "-m", "skill symlink fixture")
			git(dir, "clone", "--bare", "--template=", work, remote)
			opts.Executor = &localGitProbe{fake: fake, local: runner.NewLocal(runner.Options{}), remote: remote, gitConfig: "/dev/null"}
			report := doctor.Check(context.Background(), path, opts)
			if tc.code != "" {
				requireFinding(t, report, tc.severity, tc.code, "octo/repo implementer")
			} else if len(report.Findings) != 0 {
				t.Fatalf("available repository skill: %+v", report)
			}
		})
	}
}
