package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/maintenance"
	"github.com/rcpassos/mergeyard/internal/review"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestCLIControlsUseOwningRuntimeAndWatchReadOnly(t *testing.T) {
	root := t.TempDir()
	token := "runtime-token"
	mux := http.NewServeMux()
	paused := false
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(web.Status{Workspace: root, Paused: paused, Token: token, Runs: []workflow.Run{{ID: "run-1", Repository: "owner/repo", IssueNumber: 7, State: workflow.Active, Phase: workflow.Implement, SessionRecoveries: []workflow.SessionRecovery{{Phase: workflow.Implement, Role: "implementer", Round: 0, PreviousSessionID: "missing-session", SessionID: "codex-session"}}, Implementer: &workflow.ImplementSnapshot{Agent: "codex", SessionID: "codex-session", Model: "chosen-model", Effort: "medium", Skills: []string{"implement"}, Permissions: "workspace-write · network true · approvals never", Attempt: 2, Status: "running", ProcessSession: "live-phase"}}, {ID: "merged-run", Repository: "owner/repo", IssueNumber: 8, State: workflow.Completed, Merge: &maintenance.Snapshot{Early: true, ProcessExited: true, Error: "dirty work preserved"}}, {ID: "old-run", State: workflow.Completed}}})

	})
	mux.HandleFunc("POST /scheduler/{action}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "http://"+r.Host || r.Header.Get("X-CSRF-Token") != token {
			http.Error(w, "missing CLI CSRF", 403)
			return
		}
		paused = r.PathValue("action") == "pause"
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/runs/run-1/watch", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(runner.SessionRef{Name: "live-phase"})
	})
	stopped := false
	mux.HandleFunc("POST /api/runs/run-1/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "http://"+r.Host || r.Header.Get("X-CSRF-Token") != token {
			http.Error(w, "missing CLI CSRF", 403)
			return
		}
		stopped = true
		w.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	path := filepath.Join(root, "config.yaml")
	port := strings.TrimPrefix(server.URL, "http://127.0.0.1:")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("workspace: %q\nport: %s\n", root, port)), 0600); err != nil {
		t.Fatal(err)
	}
	tools := t.TempDir()
	if err := os.WriteFile(filepath.Join(tools, "tmux"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools)
	for _, action := range []string{"status", "pause", "status", "resume", "stop", "watch"} {
		args := []string{action, "--config", path}
		if action == "watch" || action == "stop" {
			args = append(args, "run-1")
		}
		code, out, errOut := runCLI(args...)
		if code != 0 {
			t.Fatalf("%s: exit %d %s", action, code, errOut)
		}
		if action == "status" && (!strings.Contains(out, "owner/repo#7  ACTIVE/implement") || strings.Contains(out, "old-run")) {
			t.Fatalf("status = %s", out)
		}
		if action == "status" {
			for _, text := range []string{"Merged — cleanup pending", "Early merge before readiness", "Remove running label", "dirty work preserved", "implementer codex", "chosen-model", "medium", "implement", "workspace-write", "codex-session", "attempt 2: running", "live-phase", "harness.session_resume_failed", "missing-session", "continuing once in a fresh session", "implementer active session codex-session"} {

				if !strings.Contains(out, text) {
					t.Fatalf("missing implementer status %q: %s", text, out)
				}
			}
		}
		if action == "watch" && out != "-L\nmergeyard\nattach-session\n-r\n-t\n=live-phase\n" {
			t.Fatalf("watch did not attach read-only: %q", out)
		}
	}
	// A different CLI PATH can lack tmux even though the daemon has a live phase.
	if err := os.Remove(filepath.Join(tools, "tmux")); err != nil {
		t.Fatal(err)
	}
	codeMissing, _, errMissing := runCLI("watch", "run-1", "--config", path)
	if codeMissing != 1 || !strings.Contains(errMissing, "phase.watch_failed") {
		t.Fatalf("watch missing tmux lacks stable code: %d %s", codeMissing, errMissing)
	}
	if paused || !stopped {
		t.Fatalf("controls not delivered: paused %v stopped %v", paused, stopped)
	}
	// Another workspace on this port cannot receive commands for this config.
	if err := os.WriteFile(path, []byte(fmt.Sprintf("workspace: %q\nport: %s\n", filepath.Join(root, "other"), port)), 0600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runCLI("pause", "--config", path)
	if code != 1 || !strings.Contains(errOut, "workspace.mismatch") {
		t.Fatalf("wrong runtime: %d %s", code, errOut)
	}
	if paused {
		t.Fatal("changed another workspace's scheduler")
	}
}

func TestCLIReportsRuntimeUnavailable(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	port := strings.TrimPrefix(server.URL, "http://127.0.0.1:")
	server.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("port: "+port+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err := connect(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "internal.runtime_unavailable") {
		t.Fatalf("offline runtime: %v", err)
	}
}

func TestCLIStatusEscapesUntrustedReviewText(t *testing.T) {
	root := t.TempDir()
	attack := "\x1b[2J\x1b[H\r\nScheduler: paused\b\t\a\u009b2J\u202e"
	file := "src/file" + attack + ".go"
	raw, err := json.Marshal(review.Report{SchemaVersion: 1, Status: "approved", Summary: "Reviewed café " + attack, Findings: []review.Finding{{ID: "R1-F1" + attack, Severity: "note", Title: "Optional " + attack, Details: "Details " + attack, File: &file}}})
	if err != nil {
		t.Fatal(err)
	}
	report, err := review.Parse(raw)
	if err != nil {
		t.Fatalf("review report should retain original text: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(web.Status{Workspace: root, Runs: []workflow.Run{{ID: "run-1", Repository: "owner/repo", IssueNumber: 7, State: workflow.Active, Phase: workflow.Review, LastErrorCode: "review.failed", LastErrorMessage: attack, CI: &ci.Snapshot{SHA: "approved" + attack, CurrentHead: "current" + attack, QueryError: "query" + attack, Warning: "warning" + attack, Evidence: ci.Evidence{MergeSHA: "merge" + attack, Checks: []ci.Check{{Name: "check" + attack, SHA: "check-sha" + attack, State: "unknown", Status: "completed", Conclusion: "neutral", URL: "https://check.example/" + attack, Excerpt: "check excerpt" + attack}}, Required: []ci.Requirement{{Name: "required" + attack}}}}, FixHistory: []review.FixSnapshot{{CI: &ci.Snapshot{RepairCause: "ci.check_failed" + attack, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build" + attack, Conclusion: "failure", Excerpt: "failure excerpt" + attack, URL: "https://check.example/" + attack}}}}, Round: 1, Attempt: 1, Status: "succeeded", SessionID: "implementer" + attack, Report: &review.FixReport{SchemaVersion: 1, Status: "success", Summary: "Fix summary" + attack, Responses: []review.Response{{FindingID: "F1" + attack, Resolution: "disputed", Note: "Safe" + attack}}}}}, Review: &review.Snapshot{Agent: "claude", Model: "model" + attack, Effort: "high", Skills: []string{"review" + attack}, PermissionMode: "auto", SessionID: "session-1", Round: 1, Attempt: 1, TargetSHA: "pinned", Status: "succeeded", Accepted: true, Report: &report}}}})
	}))
	defer server.Close()
	configPath := filepath.Join(root, "config.yaml")
	port := strings.TrimPrefix(server.URL, "http://127.0.0.1:")
	if err := os.WriteFile(configPath, []byte(fmt.Sprintf("workspace: %q\nport: %s\n", root, port)), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI("status", "--config", configPath)
	if code != 0 {
		t.Fatalf("status failed: %s", errOut)
	}
	for _, r := range out {
		if (unicode.IsControl(r) && r != '\n') || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			t.Fatalf("unsafe terminal control %U survived status rendering: %q", r, out)
		}
	}
	for _, text := range []string{`\x1b[2J\x1b[H\r\nScheduler: paused\b\t\a\u009b2J\u202e`, "Reviewed café", "owner/repo#7  ACTIVE/review", "verdict approved", "CI wait", "verified test merge commit", "check-sha", "neutral", "required required", "query", "fix round 1 attempt 1", "Fix summary", "disputed: Safe", "CI repair cause: ci.check_failed", "next independent review round 2", "failure excerpt", "check excerpt"} {
		if !strings.Contains(out, text) {
			t.Fatalf("missing escaped/readable status %q: %q", text, out)
		}
	}
	if strings.Contains(out, "\nScheduler: paused") {
		t.Fatal("review text spoofed a status line")
	}
}
