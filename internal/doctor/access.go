package doctor

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rcpassos/mergeyard/internal/config"
)

func (c *checker) access(cfg config.Config) {
	if err := probeWorkspace(cfg.Workspace); err != nil {
		c.report.add(Error, "workspace.not_writable", cfg.Workspace, err.Error())
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		c.report.add(Error, "config.invalid_port", "port", "dashboard port must be between 1 and 65535")
		return
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Port))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		c.report.add(Error, "config.port_unavailable", address, err.Error())
		return
	}
	if err := listener.Close(); err != nil {
		c.report.add(Error, "config.port_unavailable", address, err.Error())
	}
}

func expandHome(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	return filepath.Abs(path)
}

// Probe the existing workspace or its nearest existing parent. A temporary
// directory and file prove access without initializing a workspace or database.
func probeWorkspace(path string) error {
	if path == "" {
		return errors.New("workspace path is empty")
	}
	dir, err := expandHome(path)
	if err != nil {
		return err
	}
	for {
		info, err := os.Stat(dir)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%s is not a directory", dir)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		// A dangling symlink already occupies this path; probing its parent
		// would wrongly claim that the workspace can be created there.
		if _, linkErr := os.Lstat(dir); linkErr == nil {
			return fmt.Errorf("%s exists but cannot be resolved: %w", dir, err)
		} else if !errors.Is(linkErr, os.ErrNotExist) {
			return linkErr
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return err
		}
		dir = parent
	}
	probe, err := os.MkdirTemp(dir, ".mergeyard-doctor-")
	if err != nil {
		return err
	}
	writeErr := os.WriteFile(filepath.Join(probe, "probe"), []byte("mergeyard doctor\n"), 0600)
	return errors.Join(writeErr, os.RemoveAll(probe))
}
