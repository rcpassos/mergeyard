package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/escalation"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestCLIStatusShowsEscalationReasonsAndWarnings(t *testing.T) {
	root := t.TempDir()
	run := workflow.Run{ID: "ready", Repository: "owner/repo", IssueNumber: 100, State: workflow.ReadyToMerge,
		Escalation: &escalation.Snapshot{HeadSHA: "approved", Reasons: []escalation.Flag{{Reason: "migration", Files: []string{"migrations/001.sql"}}}, PublicationWarning: "PR body update will retry."}}
	server := runtimeServer(t, root, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(web.Status{Workspace: root, Runs: []workflow.Run{run}})
	}))
	defer server.Close()
	port := strings.TrimPrefix(server.URL, "http://127.0.0.1:")
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("workspace: %q\nport: %s\n", root, port)), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := runCLI("status", "--config", path)
	if code != 0 {
		t.Fatalf("status: %d %s", code, stderr)
	}
	for _, text := range []string{"Waiting for your merge", "Needs careful review", "migration: migrations/001.sql", "PR body update will retry."} {
		if !strings.Contains(out, text) {
			t.Fatalf("missing %q: %s", text, out)
		}
	}
	run.Escalation = &escalation.Snapshot{HeadSHA: "approved", Reasons: []escalation.Flag{}, Published: true}
	code, out, stderr = runCLI("status", "--config", path)
	if code != 0 || strings.Contains(out, "Needs careful review") {
		t.Fatalf("unflagged status: %d %s %s", code, out, stderr)
	}
	run.Escalation.EvaluationWarning = "Escalation evaluation unavailable; inspect the diff before merging. Will retry."
	code, out, stderr = runCLI("status", "--config", path)
	if code != 0 || !strings.Contains(out, "Escalation evaluation unavailable") {
		t.Fatalf("unknown evaluation: %d %s %s", code, out, stderr)
	}
}
