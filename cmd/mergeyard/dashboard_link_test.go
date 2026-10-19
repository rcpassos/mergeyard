package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/web"
)

func assertHandoffPage(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("handoff page is not owner-only: %v %v", info, err)
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil || dir.Mode().Perm() != 0700 {
		t.Fatalf("handoff directory is not private: %v %v", dir, err)
	}
}

func TestCleanupDashboardHandoffsRemovesOnlyExpiredPages(t *testing.T) {
	root := t.TempDir()
	links, err := dashboardLinksDir(root)
	if err != nil {
		t.Fatal(err)
	}
	create := func(name string) string {
		t.Helper()
		dir := filepath.Join(links, name)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, dashboardHandoffFile), []byte("expired token"), 0600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	expired := create(dashboardHandoffPrefix + "expired")
	recent := create(dashboardHandoffPrefix + "recent")
	unrelated := create("notes")
	old := time.Now().Add(-web.DashboardTokenLifetime - time.Minute)
	if err := os.Chtimes(expired, old, old); err != nil {
		t.Fatal(err)
	}

	cleanupDashboardHandoffs(root)

	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Fatalf("expired handoff page survived cleanup: %v", err)
	}
	for _, kept := range []string{recent, unrelated} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("cleanup removed %s: %v", kept, err)
		}
	}
}
