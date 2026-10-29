package testkit

import (
	"os"
	"path/filepath"
)

// RepositoryPath locates checked-in fixtures from either scheduler test package.
func RepositoryPath(path string) string {
	dir, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, path)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic("cannot locate repository fixtures")
		}
		dir = parent
	}
}
