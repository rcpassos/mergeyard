package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rcpassos/mergeyard/internal/fault"
)

// DefaultPath is the PRD §10 workspace default used by configuration and Open.
const DefaultPath = "~/.mergeyard"

// Workspace holds the root of the managed layout described in PRD §10.
type Workspace struct {
	Root string
}

// Open creates the workspace directories without changing existing artifacts.
// An empty path uses ~/.mergeyard; a leading ~/ is expanded for configured paths.
func Open(path string) (*Workspace, error) {
	if path == "" {
		path = DefaultPath
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, &fault.Error{Code: "workspace.home", Path: path, Err: fmt.Errorf("resolve workspace home: %w", err)}
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	root, err := filepath.Abs(path)
	if err != nil {
		return nil, &fault.Error{Code: "workspace.path", Path: path, Err: fmt.Errorf("resolve workspace path: %w", err)}
	}
	for _, dir := range []string{root, filepath.Join(root, "repos"), filepath.Join(root, "worktrees"), filepath.Join(root, "runs")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, &fault.Error{Code: "workspace.create", Path: dir, Err: fmt.Errorf("create workspace directory: %w", err)}
		}
	}
	return &Workspace{Root: root}, nil
}

func (w *Workspace) DatabasePath() string { return filepath.Join(w.Root, "state.db") }

func (w *Workspace) LockPath() string { return filepath.Join(w.Root, "mergeyard.lock") }
