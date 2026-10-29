package testkit

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workspace"
)

// Each integration scenario owns its Git configuration. Registration follows
// the test hierarchy, so helpers outside the workspace use the same identity.
// Subprocess control planes recover the configuration beside their workspace.
var FixtureGitConfigs = struct {
	sync.Mutex
	Paths map[string]string
}{Paths: map[string]string{}}

func RegisterGitConfig(t *testing.T, path string) {
	t.Helper()
	FixtureGitConfigs.Lock()
	FixtureGitConfigs.Paths[t.Name()] = path
	FixtureGitConfigs.Unlock()
	t.Cleanup(func() {
		FixtureGitConfigs.Lock()
		delete(FixtureGitConfigs.Paths, t.Name())
		FixtureGitConfigs.Unlock()
	})
}
func TestGitConfig(t *testing.T) string {
	t.Helper()
	FixtureGitConfigs.Lock()
	defer FixtureGitConfigs.Unlock()
	for name := t.Name(); ; {
		if path := FixtureGitConfigs.Paths[name]; path != "" {
			return path
		}
		index := strings.LastIndexByte(name, '/')
		if index < 0 {
			break
		}
		name = name[:index]
	}
	return os.Getenv("GIT_CONFIG_GLOBAL")
}
func FixtureConfigAt(path string) string {
	for dir := path; ; dir = filepath.Dir(dir) {
		configPath := filepath.Join(dir, "gitconfig")
		if info, err := os.Stat(configPath); err == nil && !info.IsDir() {
			return configPath
		}
		if filepath.Dir(dir) == dir {
			return ""
		}
	}
}
func TestGitEnvironment(t *testing.T, dir string) []string {
	path := TestGitConfig(t)
	if path == "" {
		path = FixtureConfigAt(dir)
	}
	if path == "" {
		return nil
	}
	return []string{"GIT_CONFIG_GLOBAL=" + path, "GIT_CONFIG_NOSYSTEM=1"}
}
func TestGitManager(w *workspace.Workspace) *managedgit.Manager {
	return TestGitManagerWithTimeouts(w, managedgit.Timeouts{})
}
func TestGitManagerWithTimeouts(w *workspace.Workspace, timeouts managedgit.Timeouts) *managedgit.Manager {
	env := map[string]string{}
	if path := FixtureConfigAt(w.Root); path != "" {
		env["GIT_CONFIG_GLOBAL"] = path
		env["GIT_CONFIG_NOSYSTEM"] = "1"
	}
	return managedgit.NewWithOptions(w, managedgit.Options{Timeouts: timeouts, Environment: env})
}
func TestScheduler(cfg config.Config, resources scheduler.Resources, deps scheduler.Dependencies) (*scheduler.Scheduler, error) {
	if deps.Git == nil && resources.Workspace != nil {
		deps.Git = TestGitManager(resources.Workspace)
	}
	return scheduler.New(cfg, resources, deps)
}
