package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"

	"github.com/rcpassos/mergeyard/internal/store"
	"github.com/rcpassos/mergeyard/internal/workspace"
)

// ErrWorkspaceLocked means another process already owns this workspace.
var ErrWorkspaceLocked = errors.New("workspace.locked: workspace is held by another mergeyard process")

// Runtime owns a workspace lock and its migrated runtime database.
type Runtime struct {
	Workspace *workspace.Workspace
	DB        *sql.DB
	lock      *os.File
	closeOnce sync.Once
	closeErr  error
}

// Open acquires exclusive workspace ownership before opening the database.
func Open(ctx context.Context, path string) (*Runtime, error) {
	w, err := workspace.Open(path)
	if err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(w.LockPath(), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("workspace.lock_open: open workspace lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("%w (%s)", ErrWorkspaceLocked, w.LockPath())
		}
		return nil, fmt.Errorf("workspace.lock_acquire: acquire workspace lock %s: %w", w.LockPath(), err)
	}
	db, err := store.Open(ctx, w.DatabasePath())
	if err != nil {
		lock.Close()
		return nil, err
	}
	return &Runtime{Workspace: w, DB: db, lock: lock}, nil
}

// Close closes SQLite before releasing ownership. The lock file stays on disk:
// unlinking it could let two processes lock different inodes for the same path.
func (r *Runtime) Close() error {
	r.closeOnce.Do(func() {
		r.closeErr = errors.Join(r.DB.Close(), r.lock.Close())
	})
	return r.closeErr
}
