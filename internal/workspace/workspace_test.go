package workspace_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/workspace"
)

func TestCreateLayoutAndRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	w, err := workspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if w.Root != root || w.DatabasePath() != filepath.Join(root, "state.db") {
		t.Fatalf("unexpected workspace paths: %+v, %s", w, w.DatabasePath())
	}
	for _, name := range []string{"repos", "worktrees", "runs"} {
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil || !info.IsDir() {
			t.Fatalf("missing directory %s: %v", name, err)
		}
	}
	artifact := filepath.Join(root, "runs", "existing-run")
	if err := os.WriteFile(artifact, []byte("preserve me"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Open(root); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(artifact); err != nil || string(data) != "preserve me" {
		t.Fatalf("restart changed an artifact: %q, %v", data, err)
	}
}

func TestCreateFailureHasCodePathAndUnderlyingCause(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(file, "workspace")
	_, err := workspace.Open(root)
	var failure *fault.Error
	if !errors.As(err, &failure) || failure.Code != "workspace.create" || failure.Path != root {
		t.Fatalf("expected structured workspace failure, got %v", err)
	}
	var pathError *os.PathError
	if !errors.As(err, &pathError) {
		t.Fatalf("lost filesystem cause: %v", err)
	}
}

func TestDefaultAndConfiguredHomePaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for input, want := range map[string]string{
		"":           filepath.Join(home, ".mergeyard"),
		"~":          home,
		"~/custom":   filepath.Join(home, "custom"),
		"~/custom/~": filepath.Join(home, "custom", "~"),
	} {
		w, err := workspace.Open(input)
		if err != nil {
			t.Fatal(err)
		}
		if w.Root != want {
			t.Errorf("Open(%q).Root = %q, want %q", input, w.Root, want)
		}
	}
}
