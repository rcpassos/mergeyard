package app_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/fault"
)

func TestWorkspaceOwnershipAndRelease(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "workspace")
	first, err := app.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Close() })
	if second, err := app.Open(ctx, root); err == nil {
		second.Close()
		t.Fatal("second owner acquired the workspace")
	} else if !errors.Is(err, app.ErrWorkspaceLocked) {
		t.Fatalf("expected lock error, got %v", err)
	} else {
		var failure *fault.Error
		if !errors.As(err, &failure) || failure.Code != "workspace.locked" || failure.Path != filepath.Join(root, "mergeyard.lock") {
			t.Fatalf("expected structured lock error, got %v", err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := app.Open(ctx, root)
	if err != nil {
		t.Fatalf("workspace not released: %v", err)
	}
	defer second.Close()
	if _, err := os.Stat(filepath.Join(root, "state.db")); err != nil {
		t.Fatalf("database not created: %v", err)
	}
}

func TestFailedStartupReleasesLock(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state.db")
	if err := os.WriteFile(path, []byte("not a SQLite database"), 0600); err != nil {
		t.Fatal(err)
	}
	if runtime, err := app.Open(context.Background(), root); err == nil {
		runtime.Close()
		t.Fatal("corrupt database opened successfully")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	runtime, err := app.Open(context.Background(), root)
	if err != nil {
		t.Fatalf("failed startup retained the lock: %v", err)
	}
	defer runtime.Close()
}
