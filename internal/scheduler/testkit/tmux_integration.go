package testkit

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/sessions"
)

var TmuxScenarioSlots = make(chan struct{}, 16)

// tmuxFlow is the real Git, native harness and process integration fixture.
// Scheduler policy tests use newScenario instead.
func TmuxFlow(t *testing.T, script string) (*scheduler.Scheduler, *app.Runtime, *FakeGitHub, string, config.Config, runner.Runner) {
	t.Helper()
	select {
	case TmuxScenarioSlots <- struct{}{}:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	t.Cleanup(func() { <-TmuxScenarioSlots })
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux required")
	}
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	GitCommand(t, root, "init", "--bare", "--initial-branch=main", remote)
	GitCommand(t, root, "init", "--initial-branch=main", seed)
	configPath := filepath.Join(root, "gitconfig")
	GitCommand(t, root, "config", "--file", configPath, "user.name", "Mergeyard Test")
	GitCommand(t, root, "config", "--file", configPath, "user.email", "mergeyard-test@example.com")
	GitCommand(t, root, "config", "--file", configPath, "url."+remote+".insteadOf", "https://github.com/owner/repo.git")
	RegisterGitConfig(t, configPath)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("initial\n"), 0600); err != nil {
		t.Fatal(err)
	}
	GitCommand(t, seed, "add", ".")
	GitCommand(t, seed, "commit", "-m", "initial")
	GitCommand(t, seed, "remote", "add", "origin", remote)
	GitCommand(t, seed, "push", "origin", "main")
	if !strings.Contains(script, "review-") {
		script = `case "$*" in *review-*)
` + ApprovedReview + `
;; *)
` + script + `
;; esac`
	}
	executable := filepath.Join(root, "fake-claude")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	runtime, err := app.Open(context.Background(), filepath.Join(root, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runtime.Close() })
	cfg, _, err := config.Parse([]byte("repositories:\n  - repo: owner/repo\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Agents.Claude.Executable = executable
	socket := fmt.Sprintf("mergeyard-scheduler-test-%d", time.Now().UnixNano())
	t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })
	r := runner.NewLocal(runner.Options{SocketName: socket})
	t.Cleanup(func() { StopFixtureSessions(t, runtime.Workspace.Root, socket) })
	api := &FakeGitHub{Issues: map[string][]github.Issue{"owner/repo": {Ready(7)}}, Head: func(branch string) string { return GitCommand(t, remote, "rev-parse", "refs/heads/"+branch) }}
	s, err := TestScheduler(cfg, SchedulerResources(runtime), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	return s, runtime, api, remote, cfg, r
}

// StopFixtureSessions verifies owned process groups have exited before TempDir
// cleanup. Killing the server alone can leave a wrapper writing its last files.
func StopFixtureSessions(t *testing.T, root, socket string) {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(root, "runs", "*", "phases", "*"))
	if err != nil {
		t.Error(err)
		return
	}
	checks, err := filepath.Glob(filepath.Join(root, "harness-checks", "*"))
	if err != nil {
		t.Error(err)
		return
	}
	dirs = append(dirs, checks...)
	cleanup := runner.NewLocal(runner.Options{SocketName: socket, GracePeriod: 50 * time.Millisecond})
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, "process-group.json")); os.IsNotExist(err) {
			continue
		} else if err != nil {
			t.Error(err)
			continue
		}
		req := runner.SessionRequest{PhaseDir: dir}
		if filepath.Base(filepath.Dir(dir)) == "harness-checks" {
			req.RunID = filepath.Base(dir)
			req.Phase = "harness-check"
			req.Attempt = 1
		} else {
			parts := strings.Split(filepath.Base(dir), "-")
			if len(parts) < 3 {
				t.Errorf("unexpected phase directory %s", dir)
				continue
			}
			req.RunID = filepath.Base(filepath.Dir(filepath.Dir(dir)))
			req.Phase = strings.Join(parts[:len(parts)-2], "-")
			req.Round, err = strconv.Atoi(parts[len(parts)-2])
			if err != nil {
				t.Error(err)
				continue
			}
			req.Attempt, err = strconv.Atoi(parts[len(parts)-1])
			if err != nil {
				t.Error(err)
				continue
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := cleanup.StopSession(ctx, runner.SessionRef{Name: sessions.Name(req), PhaseDir: dir})
		cancel()
		if err != nil {
			t.Errorf("stop owned fixture session: %v", err)
		}
	}
}
