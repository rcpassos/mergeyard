package sessions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func environmentPath(phaseDir string) string {
	return filepath.Join(phaseDir, "environment.sh")
}

// A transient owner-only file avoids exposing credentials in tmux/pane argv
// and bypasses tmux's command-size limit. The wrapper unlinks it before launch.
func writeEnvironment(command Command, path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	var script strings.Builder
	script.WriteString("set --")
	for _, value := range command.Environment() {
		script.WriteByte(' ')
		script.WriteString(quote(value))
	}
	script.WriteByte('\n')
	_, writeErr := file.WriteString(script.String())
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return errors.Join(err, removeEnvironment(path))
	}
	return nil
}

func removeEnvironment(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return failure("phase.environment_failed", path, err)
}

// Start does not return with an environment file pending consumption. Use an
// independent bound because tmux may commit a launch before client cancellation.
func waitEnvironment(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
