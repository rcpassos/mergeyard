// Package doctor checks machine and repository prerequisites without changing
// managed checkouts or remote state.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/runner"
)

type Severity string

const (
	Error        Severity = "error"
	Warning      Severity = "warning"
	Unverifiable Severity = "unverifiable"
)

// Finding identifies a failed or inconclusive check. Scope is a machine tool,
// configuration path, or repository (optionally including its role).
type Finding struct {
	Severity Severity
	Code     string
	Scope    string
	Message  string
}

type Report struct{ Findings []Finding }

func (r Report) HasErrors() bool {
	for _, finding := range r.Findings {
		if finding.Severity == Error {
			return true
		}
	}
	return false
}

// Write groups findings in a stable order, including empty severity groups.
func (r Report) Write(w io.Writer) error {
	for _, severity := range []Severity{Error, Warning, Unverifiable} {
		if _, err := fmt.Fprintf(w, "%s:\n", severity); err != nil {
			return err
		}
		found := false
		for _, finding := range r.Findings {
			if finding.Severity != severity {
				continue
			}
			found = true
			if _, err := fmt.Fprintf(w, "  [%s] %s: %s\n", finding.Code, finding.Scope, finding.Message); err != nil {
				return err
			}
		}
		if !found {
			if _, err := fmt.Fprintln(w, "  none"); err != nil {
				return err
			}
		}
	}
	return nil
}

// Executor is the external-command boundary; runner.Local implements it.
type Executor interface {
	Exec(context.Context, runner.ExecRequest) (runner.ExecResult, error)
}

type Options struct {
	Executor Executor
	// EffectiveUID defaults to os.Geteuid. Tests can simulate the root check.
	EffectiveUID func() int
	// CommandTimeout bounds each external check; zero uses 30 seconds.
	CommandTimeout time.Duration
}

// Check loads the same effective configuration used by the CLI and reports
// diagnostics. Invalid configuration prevents dependent checks.
func Check(ctx context.Context, path string, options Options) Report {
	cfg, doc, err := config.Load(path)
	var report Report
	if err != nil {
		code := "config.read_failed"
		var coded *fault.Error
		if errors.As(err, &coded) {
			code = coded.Code
		}
		report.add(Error, code, "configuration", err.Error())
		return report
	}
	for _, warning := range doc.Warnings {
		report.add(Warning, warning.Code, warning.Path, warning.Message)
	}
	if options.Executor == nil {
		options.Executor = runner.NewLocal(runner.Options{})
	}
	if options.EffectiveUID == nil {
		options.EffectiveUID = os.Geteuid
	}
	if options.CommandTimeout <= 0 {
		options.CommandTimeout = 30 * time.Second
	}
	c := checker{ctx: ctx, options: options, report: &report, env: make(map[string]string)}
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		c.env[key] = value
	}
	c.env["GIT_TERMINAL_PROMPT"], c.env["GH_PROMPT_DISABLED"], c.env["LC_ALL"] = "0", "1", "C"
	c.access(cfg)
	c.machine(cfg)
	c.capabilities(cfg)
	for _, repo := range cfg.Repositories {
		c.repository(repo)
	}
	return report
}

func (r *Report) add(severity Severity, code, scope, message string) {
	r.Findings = append(r.Findings, Finding{severity, code, scope, message})
}

type checker struct {
	ctx      context.Context
	options  Options
	report   *Report
	env      map[string]string
	tools    map[string]bool
	versions map[string]version
}

func (c *checker) command(dir, executable string, args ...string) (runner.ExecResult, error) {
	return c.commandInput(dir, "", executable, args...)
}

func (c *checker) commandInput(dir, inputPath, executable string, args ...string) (runner.ExecResult, error) {
	ctx, cancel := context.WithTimeout(c.ctx, c.options.CommandTimeout)
	defer cancel()
	env := c.env
	if executable == "git" {
		// Keep credential helpers and transport rewrites, but do not let a
		// calling shell redirect probes into its repository or index. Hooks
		// must not run, including reference-transaction hooks during fetch.
		env = make(map[string]string, len(c.env))
		for key, value := range c.env {
			env[key] = value
		}
		for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE", "GIT_SHALLOW_FILE"} {
			delete(env, key)
		}
		args = append([]string{"-c", "core.hooksPath=/dev/null"}, args...)
	}
	result, err := c.options.Executor.Exec(ctx, runner.ExecRequest{Executable: executable, Args: args, Dir: dir, Env: env, StdinPath: inputPath})
	if err != nil {
		return result, err
	}
	if result.ExitCode != 0 {
		// Auth output can contain account details; findings use stable messages
		// rather than echoing arbitrary command stdout/stderr.
		return result, fmt.Errorf("%s exited with status %d", executable, result.ExitCode)
	}
	return result, nil
}

func (c *checker) machine(cfg config.Config) {
	c.tools = make(map[string]bool)
	c.versions = make(map[string]version)
	for _, tool := range []struct{ name, flag string }{{"git", "--version"}, {"tmux", "-V"}, {"gh", "--version"}} {
		if _, err := c.command("", tool.name, tool.flag); err != nil {
			c.report.add(Error, "internal.tool_missing", tool.name, "tool is missing or cannot run: "+err.Error())
		} else {
			c.tools[tool.name] = true
		}
	}
	if c.tools["gh"] {
		if _, err := c.command("", "gh", "auth", "status", "--hostname", "github.com"); err != nil {
			c.report.add(Error, "github.not_logged_in", "gh", "GitHub authentication failed; run gh auth login: "+err.Error())
		}
	}
	assigned := make(map[string]bool)
	for _, assignedRole := range roles(cfg) {
		assigned[assignedRole.role.Agent] = true
	}
	for _, agent := range []string{"claude", "codex"} {
		if !assigned[agent] {
			continue
		}
		execPath := executable(cfg, agent)
		result, err := c.command("", execPath, "--version")
		if err != nil {
			c.report.add(Error, "harness.not_found", agent, "assigned harness is missing or cannot run: "+err.Error())
			continue
		}
		c.tools[agent] = true
		minimum := claudeMinimum
		auth := []string{"auth", "status"}
		if agent == "codex" {
			minimum, auth = codexMinimum, []string{"login", "status"}
		}
		v, ok := parseVersion(string(result.Stdout) + " " + string(result.Stderr))
		if !ok {
			c.report.add(Unverifiable, "harness.version_unverifiable", agent, "cannot parse the installed version; minimum required: "+minimum.String())
		} else {
			c.versions[agent] = v
			if v.less(minimum) {
				c.report.add(Error, "harness.version_unsupported", agent, "installed "+v.String()+"; minimum required: "+minimum.String())
			}
		}
		if _, err := c.command("", execPath, auth...); err != nil {
			c.report.add(Error, "harness.not_logged_in", agent, "harness login check failed; log in with the configured executable: "+err.Error())
		}
	}
	if assigned["claude"] && cfg.Agents.Claude.PermissionMode == "bypassPermissions" && c.options.EffectiveUID() == 0 {
		c.report.add(Error, "harness.root_bypass_permissions", "claude", "bypassPermissions cannot run as root; run as a non-root user or change agents.claude.permission_mode")
	}
}
