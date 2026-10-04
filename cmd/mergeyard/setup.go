package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/repository"
)

// setupPrompts keeps one reader for the whole dialogue so piped answers are not
// discarded by successive buffered readers. EOF never means confirmation.
type setupPrompts struct {
	ctx    context.Context
	input  *bufio.Reader
	output io.Writer
}

func (p setupPrompts) ask(question string) (string, error) {
	if err := p.ctx.Err(); err != nil {
		return "", err
	}
	if _, err := fmt.Fprint(p.output, question+": "); err != nil {
		return "", err
	}
	type reply struct {
		answer string
		err    error
	}
	// A terminal read cannot be cancelled through context. The buffered channel
	// lets the reader finish if input arrives after cancellation; the CLI exits
	// immediately without waiting for it or reusing the reader.
	replies := make(chan reply, 1)
	go func() {
		answer, err := p.input.ReadString('\n')
		replies <- reply{answer, err}
	}()
	select {
	case <-p.ctx.Done():
		return "", p.ctx.Err()
	case result := <-replies:
		if err := p.ctx.Err(); err != nil {
			return "", err
		}
		if result.err != nil && !(result.err == io.EOF && len(result.answer) > 0) {
			return "", fmt.Errorf("read answer: %w", result.err)
		}
		return strings.TrimSpace(result.answer), nil
	}
}

func (p setupPrompts) confirm(question string) (bool, error) {
	for {
		answer, err := p.ask(question + " [y/N]")
		if err != nil {
			return false, err
		}
		switch strings.ToLower(answer) {
		case "y", "yes":
			return true, nil
		case "", "n", "no":
			return false, nil
		}
		fmt.Fprintln(p.output, "Enter yes or no.")
	}
}

func (p setupPrompts) agent(role string, installed []string) (string, error) {
	for {
		answer, err := p.ask(fmt.Sprintf("%s agent (%s) [%s]", role, strings.Join(installed, ", "), installed[0]))
		if err != nil {
			return "", err
		}
		if answer == "" {
			return installed[0], nil
		}
		for i, name := range installed {
			if answer == name || answer == strconv.Itoa(i+1) {
				return name, nil
			}
		}
		fmt.Fprintln(p.output, "Choose an installed agent from the list.")
	}
}

// init uses the config search order, creating the home config on first setup.
// Check existence independently of parsing so even a malformed file is never
// touched before the overwrite prompt.
func setupPath(path string) (string, bool, error) {
	paths := []string{path}
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false, err
		}
		paths = []string{"mergeyard.yaml", filepath.Join(home, ".config", "mergeyard", "config.yaml")}
	}
	for _, candidate := range paths {
		if _, err := os.Lstat(candidate); err == nil {
			return candidate, true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", false, err
		}
	}
	return paths[len(paths)-1], false, nil
}

func initialize(ctx context.Context, path string, input io.Reader, output io.Writer) (string, error) {
	path, exists, err := setupPath(path)
	if err != nil {
		return "", err
	}
	prompts := setupPrompts{ctx, bufio.NewReader(input), output}
	if exists {
		confirmed, err := prompts.confirm("Overwrite setup settings in existing config " + path + " (other settings are preserved)?")
		if err != nil {
			return "", err
		}
		if !confirmed {
			fmt.Fprintln(output, "Setup cancelled; config unchanged.")
			return "", nil
		}
	}
	var cfg config.Config
	var doc *config.Document
	if exists {
		cfg, doc, err = config.Load(path)
	} else {
		cfg, doc, err = config.Parse([]byte("{}"))
	}
	if err != nil {
		return "", err
	}
	var agents, missing []string
	for _, tool := range []string{"git", "gh", "tmux", "claude", "codex"} {
		_, err := exec.LookPath(tool)
		state := "installed"
		if err != nil {
			state = "missing"
		}
		fmt.Fprintf(output, "%s: %s\n", tool, state)
		if tool == "claude" || tool == "codex" {
			if err == nil {
				agents = append(agents, tool)
			}
		} else if err != nil {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		return "", &fault.Error{Code: "internal.tool_missing", Err: fmt.Errorf("install required tools: %s", strings.Join(missing, ", "))}
	}
	if len(agents) == 0 {
		return "", &fault.Error{Code: "harness.not_found", Err: errors.New("install Claude Code or Codex before running init")}
	}
	implementer, err := prompts.agent("Implementer", agents)
	if err != nil {
		return "", err
	}
	reviewer, err := prompts.agent("Reviewer", agents)
	if err != nil {
		return "", err
	}
	var repo string
	for {
		repo, err = prompts.ask("First repository (owner/repo)")
		if err != nil {
			return "", err
		}
		if repository.ValidName(repo) {
			break
		}
		fmt.Fprintln(output, "Enter a repository as owner/repo.")
	}
	if err := validateSetupRepo(ctx, repo); err != nil {
		return "", err
	}
	if err := doc.Set([]string{"implementer", "agent"}, implementer); err != nil {
		return "", err
	}
	if err := doc.Set([]string{"reviewer", "agent"}, reviewer); err != nil {
		return "", err
	}
	labels := cfg.Labels
	found := false
	for _, existing := range cfg.Repositories {
		if strings.EqualFold(existing.Repo, repo) {
			found, labels = true, existing.Labels
			break
		}
	}
	if !found {
		if err := doc.Set([]string{"repositories", "-"}, map[string]string{"repo": repo}); err != nil {
			return "", err
		}
	}
	if err := prompts.offerLabels(repo, labels); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := doc.Write(path); err != nil {
		return "", err
	}
	fmt.Fprintf(output, "Config saved to %s. Running doctor.\n", path)
	return path, nil
}

func setupGH(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1")
	var output, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, &fault.Error{Code: "github.command_failed", Err: fmt.Errorf("gh %s failed: %w; check gh auth status and repository access", args[0], err)}
	}
	return output.Bytes(), nil
}

func validateSetupRepo(ctx context.Context, repo string) error {
	if !repository.ValidName(repo) {
		return &fault.Error{Code: "config.invalid_repo", Err: errors.New("expected owner/repo")}
	}
	_, err := setupGH(ctx, "api", "--hostname", "github.com", "--method", "GET", "repos/"+repo)
	return err
}

func createSetupLabels(ctx context.Context, repo string, labels config.Labels) error {
	data, err := setupGH(ctx, "api", "--hostname", "github.com", "--method", "GET", "--paginate", "repos/"+repo+"/labels?per_page=100")
	if err != nil {
		return err
	}
	names := make(map[string]bool)
	decoder := json.NewDecoder(bytes.NewReader(data))
	pages := 0
	for {
		var page []struct {
			Name string `json:"name"`
		}
		err := decoder.Decode(&page)
		if err == io.EOF {
			break
		}
		if err != nil || page == nil {
			return &fault.Error{Code: "github.invalid_response", Err: errors.New("expected label arrays from GitHub")}
		}
		pages++
		for _, label := range page {
			if label.Name == "" {
				return &fault.Error{Code: "github.invalid_response", Err: errors.New("GitHub returned a label without a name")}
			}
			names[strings.ToLower(label.Name)] = true
		}
	}
	if pages == 0 {
		return &fault.Error{Code: "github.invalid_response", Err: errors.New("GitHub returned no label array")}
	}
	for _, label := range []struct{ name, color, description string }{
		{labels.Ready, "0e8a16", "Ready for Mergeyard to implement"},
		{labels.Running, "1d76db", "Mergeyard agent is running"},
		{labels.NeedsAttention, "d93f0b", "Mergeyard needs human attention"},
	} {
		if names[strings.ToLower(label.name)] {
			continue
		}
		if _, err := setupGH(ctx, "label", "create", label.name, "--repo", "github.com/"+repo, "--color", label.color, "--description", label.description); err != nil {
			return err
		}
	}
	return nil
}

func addRepository(ctx context.Context, path, repo string, input io.Reader, output io.Writer) error {
	if !repository.ValidName(repo) {
		return &fault.Error{Code: "config.invalid_repo", Err: errors.New("expected owner/repo")}
	}
	cfg, doc, err := config.Load(path)
	if err != nil {
		return err
	}
	for _, existing := range cfg.Repositories {
		if strings.EqualFold(existing.Repo, repo) {
			return &fault.Error{Code: "config.duplicate_repo", Err: fmt.Errorf("%s is already configured", repo)}
		}
	}
	if err := validateSetupRepo(ctx, repo); err != nil {
		return err
	}
	if err := doc.Set([]string{"repositories", "-"}, map[string]string{"repo": repo}); err != nil {
		return err
	}
	prompts := setupPrompts{ctx, bufio.NewReader(input), output}
	if err := prompts.offerLabels(repo, cfg.Labels); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := doc.Write(doc.Path); err != nil {
		return err
	}
	fmt.Fprintf(output, "Added %s to %s.\n", repo, doc.Path)
	return nil
}

func (p setupPrompts) offerLabels(repo string, labels config.Labels) error {
	confirmed, err := p.confirm("Create missing Mergeyard labels in " + repo + "?")
	if err != nil {
		return err
	}
	if confirmed {
		return createSetupLabels(p.ctx, repo, labels)
	}
	return nil
}
