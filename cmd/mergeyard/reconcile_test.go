package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/app"
)

func TestReconcileCommandReportsOrphanAndReleasesConfiguredWorkspace(t *testing.T) {
	root := t.TempDir()
	tools := filepath.Join(root, "tools")
	if err := os.Mkdir(tools, 0700); err != nil {
		t.Fatal(err)
	}
	gh := `#!/bin/sh
case "$*" in
  *"repos/owner/repo/issues?state=open&per_page=100"*)
    printf '%s\n' '[{"number":7,"title":"Orphan","state":"open","created_at":"2026-10-01T00:00:00Z","labels":[{"name":"agent-running"},{"name":"ready-for-agent"}]}]'
    ;;
  *) echo "unexpected GitHub mutation" >&2; exit 1;;
esac
`
	for name, script := range map[string]string{"gh": gh, "tmux": "#!/bin/sh\necho 'no server running' >&2\nexit 1\n"} {
		if err := os.WriteFile(filepath.Join(tools, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	workspace := filepath.Join(root, "configured-workspace")
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte("workspace: "+strconv.Quote(workspace)+"\nrepositories:\n  - repo: owner/repo\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLI("reconcile", "--config", path)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "reconcile.orphaned_claim: owner/repo#7") || !strings.Contains(stdout, "Reconciliation complete: 1 finding(s).") {
		t.Fatalf("reconcile output: code=%d, stdout=%q, stderr=%q", code, stdout, stderr)
	}
	runtime, err := app.Open(context.Background(), workspace)
	if err != nil {
		t.Fatalf("command retained workspace lock: %v", err)
	}
	defer runtime.Close()
	history, err := runtime.Events.History(context.Background(), 0, 100)
	if err != nil || len(history) != 1 || history[0].Type != "reconcile.orphaned_claim" {
		t.Fatalf("command dispatched an orphan: events=%+v, err=%v", history, err)
	}
}
