package testkit

import (
	"os"
	"path/filepath"
	"testing"
)

func LoginHomeWithFailingLogout(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".bash_profile"), []byte("export STARTUP_STATE=ready\nstartup_function() { printf startup-ready; }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".bash_logout"), []byte("/usr/bin/false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return home
}
