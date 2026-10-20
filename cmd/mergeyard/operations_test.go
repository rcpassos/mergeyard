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
	"time"
	"unicode"

	"github.com/rcpassos/mergeyard/internal/acceptance"
	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/maintenance"
	"github.com/rcpassos/mergeyard/internal/review"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/runtimeauth"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestCLIControlsUseOwningRuntimeAndWatchReadOnly(t *testing.T) {
	root := t.TempDir()
	token := "runtime-token"
	mux := http.NewServeMux()
	paused := false
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(web.Status{Workspace: root, Paused: paused, Token: token, Runs: []workflow.Run{{ID: "run-1", Repository: "owner/repo", IssueNumber: 7, State: workflow.Active, Phase: workflow.Implement, RoleSettings: &config.RunRoleSettings{Implementer: config.RoleSettings{Agent: "codex", HarnessSource: "label", Skills: []string{"implement"}, Tier: "high", TierLabels: []string{"MERGEYARD:IMPLEMENTER-TIER=HIGH"}}, Reviewer: config.RoleSettings{Agent: "claude", Model: "stored-review-model", Effort: "high", Skills: []string{"stored-review-skill"}, HarnessSource: "config"}}, SessionRecoveries: []workflow.SessionRecovery{{Phase: workflow.Implement, Role: "implementer", Round: 0, PreviousSessionID: "missing-session", SessionID: "codex-session"}}, Implementer: &workflow.ImplementSnapshot{Agent: "codex", SessionID: "codex-session", Model: "chosen-model", Effort: "medium", Skills: []string{"implement"}, Permissions: "workspace-write · network true · approvals never", Attempt: 2, Status: "running", ProcessSession: "live-phase"}}, {ID: "merged-run", Repository: "owner/repo", IssueNumber: 8, State: workflow.Completed, Merge: &maintenance.Snapshot{Early: true, ProcessExited: true, Error: "dirty work preserved"}}, {ID: "old-run", State: workflow.Completed}}})

	})
	mux.HandleFunc("POST /api/scheduler/{action}", func(w http.ResponseWriter, r *http.Request) {
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
	server := runtimeServer(t, root, mux)
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
			for _, text := range []string{"Role settings at claim:", "tier high", "MERGEYARD:IMPLEMENTER-TIER=HIGH", "tier none", "implementer: codex · source label · model harness default · effort harness default · skills implement", "reviewer: claude · source config · model stored-review-model · effort high · skills stored-review-skill", "Merged — cleanup pending", "Early merge before readiness", "Remove running label", "dirty work preserved", "implementer codex", "chosen-model", "medium", "implement", "workspace-write", "codex-session", "attempt 2: running", "live-phase", "harness.session_resume_failed", "missing-session", "continuing once in a fresh session", "implementer active session codex-session"} {

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
	// Another workspace on this port cannot prove this workspace's secret.
	other := filepath.Join(root, "other")
	if err := os.MkdirAll(other, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := runtimeauth.EnsureSecret(other); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf("workspace: %q\nport: %s\n", other, port)), 0600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runCLI("pause", "--config", path)
	if code != 1 || !strings.Contains(errOut, "runtime.unauthenticated") {
		t.Fatalf("wrong runtime: %d %s", code, errOut)
	}
	if paused {
		t.Fatal("changed another workspace's scheduler")
	}
	// Even a copied secret keeps the workspace identity check before controls.
	copied, err := runtimeauth.LoadSecret(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeauth.SecretPath(other), copied, 0600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut = runCLI("pause", "--config", path)
	if code != 1 || !strings.Contains(errOut, "workspace.mismatch") {
		t.Fatalf("copied secret bypassed the workspace check: %d %s", code, errOut)
	}
}

func TestCLIReportsRuntimeUnavailable(t *testing.T) {
	root := t.TempDir()
	if _, err := runtimeauth.EnsureSecret(root); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.NotFoundHandler())
	port := strings.TrimPrefix(server.URL, "http://127.0.0.1:")
	server.Close()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("workspace: %q\nport: %s\n", root, port)), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err := connect(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "internal.runtime_unavailable") {
		t.Fatalf("offline runtime: %v", err)
	}
}

func TestCLIRefusesImpostorRuntimeThatCannotProveTheSecret(t *testing.T) {
	root := t.TempDir()
	secret, err := runtimeauth.EnsureSecret(root)
	if err != nil {
		t.Fatal(err)
	}
	impostorExecutable := filepath.Join(root, "impostor")
	if err := os.WriteFile(impostorExecutable, []byte("#!/bin/sh\nprintf compromised\n"), 0700); err != nil {
		t.Fatal(err)
	}
	requests, proofs := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get(runtimeauth.ProofHeader) != "" {
			proofs++
		}
		if strings.Contains(r.Header.Get(runtimeauth.ProofHeader), string(secret)) {
			t.Error("the CLI sent the workspace secret to the port")
		}
		switch r.URL.Path {
		case "/api/status":
			// The impostor knows the workspace path and the classic answer.
			json.NewEncoder(w).Encode(web.Status{Workspace: root, Token: "stolen"})
		case "/api/runs/run-1/takeover":
			json.NewEncoder(w).Encode(harness.InteractiveCommand{Executable: impostorExecutable, Dir: root, Args: []string{"--resume", "01a10c5b-38a9-7330-b833-04e365cb5f37"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("workspace: %q\nport: %s\n", root, strings.TrimPrefix(server.URL, "http://127.0.0.1:"))), 0600); err != nil {
		t.Fatal(err)
	}
	commandsAttempted := [][]string{{"status"}, {"pause"}, {"takeover", "run-1"}}
	for _, action := range commandsAttempted {
		code, out, errOut := runCLI(append(action, "--config", path)...)
		if code != 1 || !strings.Contains(errOut, "runtime.unauthenticated") || strings.Contains(out, "compromised") {
			t.Fatalf("impostor accepted for %v: %d %q %s", action, code, out, errOut)
		}
	}
	// Only the status probe of each command reached the impostor; no control
	// request was attempted.
	if requests != len(commandsAttempted) || proofs != requests {
		t.Fatalf("impostor saw %d requests with %d proofs", requests, proofs)
	}
}

func TestCLIRefusesRuntimeProvingTheWrongSecret(t *testing.T) {
	root := t.TempDir()
	if _, err := runtimeauth.EnsureSecret(root); err != nil {
		t.Fatal(err)
	}
	foreign, err := runtimeauth.EnsureSecret(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The impostor answers with the right workspace and a proof of its own
	// secret, so only the proof check can expose it.
	body, err := json.Marshal(web.Status{Workspace: root, Token: "other"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(runtimeauth.ProofHeader, runtimeauth.ResponseProof(foreign, r.Header.Get(runtimeauth.NonceHeader), http.StatusOK, body))
		w.Write(body)
	}))
	defer server.Close()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("workspace: %q\nport: %s\n", root, strings.TrimPrefix(server.URL, "http://127.0.0.1:"))), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI("status", "--config", path)
	if code != 1 || !strings.Contains(errOut, "runtime.unauthenticated") || strings.Contains(out, "Scheduler:") {
		t.Fatalf("foreign secret accepted: %d %q %s", code, out, errOut)
	}
}

func TestCLIStatusRequiresTheWorkspaceSecretFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("workspace: %q\nport: 7331\n", root)), 0600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runCLI("status", "--config", path)
	if code != 1 || !strings.Contains(errOut, "runtime.secret_missing") || !strings.Contains(errOut, "stop any running Mergeyard") {
		t.Fatalf("missing secret guidance: %d %s", code, errOut)
	}
}

func TestCLIStatusShowsOwningConfiguration(t *testing.T) {
	root := t.TempDir()
	// Runtime values must win over the CLI's connection-only configuration.
	cfg, _, err := config.Parse([]byte("concurrency: 3\nrepositories:\n  - repo: owner/repo\n    concurrency: 2\n    implementer:\n      agent: codex\n      model: runtime-model\n      effort: medium\n      skills: [implement]\n    reviewer:\n      model: review-model\n"))
	if err != nil {
		t.Fatal(err)
	}
	server := runtimeServer(t, root, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"workspace": root, "config_path": "/runtime/config.yaml\x1b[2J", "effective_config": cfg, "runs": []workflow.Run{}})
	}))
	defer server.Close()
	path := filepath.Join(root, "connection.yaml")
	port := strings.TrimPrefix(server.URL, "http://127.0.0.1:")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("workspace: %q\nport: %s\n", root, port)), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI("status", "--config", path)
	if code != 0 {
		t.Fatalf("status: %d %s", code, errOut)
	}
	for _, want := range []string{root, "/runtime/config.yaml\\x1b[2J", "global concurrency 3", "repository concurrency 2", "implementer codex", "runtime-model", "medium", "skills implement", "reviewer claude", "review-model"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status missing %q: %s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") || strings.Contains(out, "connection.yaml") {
		t.Fatalf("unsafe or local configuration source: %q", out)
	}
}

func TestCLIStatusEscapesUntrustedReviewText(t *testing.T) {
	root := t.TempDir()
	attack := "\x1b[2J\x1b[H\r\nScheduler: paused\b\t\a\u009b2J\u202e"
	file := "src/file" + attack + ".go"
	raw, err := json.Marshal(review.Report{SchemaVersion: 4, Criteria: []acceptance.Verdict{}, Status: "approved", Summary: "Reviewed café " + attack, PriorFindings: []review.PriorFinding{}, Findings: []review.Finding{{ID: "R1-F1" + attack, Severity: "note", Title: "Optional " + attack, Details: "Details " + attack, File: &file}}})
	if err != nil {
		t.Fatal(err)
	}
	report, err := review.Parse(raw)
	if err != nil {
		t.Fatalf("review report should retain original text: %v", err)
	}
	server := runtimeServer(t, root, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(web.Status{Workspace: root, Runs: []workflow.Run{{ID: "run-1", Repository: "owner/repo", IssueNumber: 7, State: workflow.Active, Phase: workflow.Review, LastErrorCode: "review.failed", LastErrorMessage: attack, HumanAction: &fault.HumanAction{Decision: "Choose recovery" + attack, Files: []string{file}, Options: []string{"Retry" + attack}}, CI: &ci.Snapshot{SHA: "approved" + attack, CurrentHead: "current" + attack, QueryError: "query" + attack, Warning: "warning" + attack, Evidence: ci.Evidence{MergeSHA: "merge" + attack, Checks: []ci.Check{{Name: "check" + attack, SHA: "check-sha" + attack, State: "unknown", Status: "completed", Conclusion: "neutral", URL: "https://check.example/" + attack, Excerpt: "check excerpt" + attack}}, Required: []ci.Requirement{{Name: "required" + attack}}}}, FixHistory: []review.FixSnapshot{{CI: &ci.Snapshot{RepairCause: "ci.check_failed" + attack, Evidence: ci.Evidence{Checks: []ci.Check{{Name: "build" + attack, Conclusion: "failure", Excerpt: "failure excerpt" + attack, URL: "https://check.example/" + attack}}}}, Round: 1, Attempt: 1, Status: "succeeded", SessionID: "implementer" + attack, Report: &review.FixReport{SchemaVersion: 1, Status: "success", Summary: "Fix summary" + attack, Responses: []review.Response{{FindingID: "F1" + attack, Resolution: "disputed", Note: "Safe" + attack}}}}}, Review: &review.Snapshot{Agent: "claude", Model: "model" + attack, Effort: "high", Skills: []string{"review" + attack}, PermissionMode: "auto", SessionID: "session-1", Round: 1, Attempt: 1, TargetSHA: "pinned", Status: "succeeded", Accepted: true, Report: &report}}}})
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
	for _, want := range []string{"Human action required: Choose recovery", "File: src/file", "Option: Retry"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status lost human action %q: %s", want, out)
		}
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

func TestCLIRetrySendsProtectedRequestAndDisplaysSelection(t *testing.T) {
	root := t.TempDir()
	token := "retry-token"
	requested := 0
	selected := workflow.Run{ID: "run-1", Repository: "owner/repo", IssueNumber: 7, State: workflow.Active, Phase: workflow.Fix, Retries: []workflow.RetrySnapshot{{NextState: workflow.Active, NextPhase: workflow.Fix, Round: 1, GrantedRound: 2, Deadline: "2026-10-06T13:00:00Z"}}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(web.Status{Workspace: root, Token: token, Runs: []workflow.Run{{ID: "run-1", State: workflow.Failed, RetryEligible: true, LastErrorCode: "review.max_rounds_exceeded", LastErrorMessage: "Retry to continue"}}})
	})
	mux.HandleFunc("POST /api/runs/run-1/retry", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "http://"+r.Host || r.Header.Get("X-CSRF-Token") != token {
			http.Error(w, "unauthorized", 403)
			return
		}
		requested++
		json.NewEncoder(w).Encode(selected)
	})
	server := runtimeServer(t, root, mux)
	defer server.Close()
	path := filepath.Join(root, "config.yaml")
	port := strings.TrimPrefix(server.URL, "http://127.0.0.1:")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("workspace: %q\nport: %s\n", root, port)), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI("retry", "run-1", "--config", path)
	if code != 0 || requested != 1 {
		t.Fatalf("retry: %d %d %s", code, requested, errOut)
	}
	for _, want := range []string{"Run run-1: ACTIVE/fix", "Retry selected ACTIVE/fix", "Additional review round granted: 2", "Renewed CI deadline: 2026-10-06T13:00:00Z"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q: %s", want, out)
		}
	}
	code, out, errOut = runCLI("status", "--config", path)
	if code != 0 || !strings.Contains(out, "FAILED/") || !strings.Contains(out, "mergeyard retry run-1") {
		t.Fatalf("failed runs invisible: %s %s", out, errOut)
	}
	other := filepath.Join(root, "other")
	if err := os.MkdirAll(other, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := runtimeauth.EnsureSecret(other); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf("workspace: %q\nport: %s\n", other, port)), 0600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut = runCLI("retry", "run-1", "--config", path)
	if code != 1 || !strings.Contains(errOut, "runtime.unauthenticated") || requested != 1 {
		t.Fatalf("retry touched wrong workspace: %d %s %d", code, errOut, requested)
	}
}

func TestCLIRequeueReturnsStoppedPublicationsToTheQueue(t *testing.T) {
	root := t.TempDir()
	token := "requeue-token"
	requested := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(web.Status{Workspace: root, Token: token})
	})
	mux.HandleFunc("POST /api/runs/run-1/requeue", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "http://"+r.Host || r.Header.Get("X-CSRF-Token") != token {
			http.Error(w, "unauthorized", 403)
			return
		}
		requested++
		json.NewEncoder(w).Encode(web.RequeueResult{RunID: "run-1", Requeued: 1, Publications: []scheduler.StoppedPublication{{
			Kind: "report", Label: "review report · round 1, attempt 1", Code: "github.forbidden",
			Warning: "Report is saved locally. PR comment publication cannot deliver this comment.",
		}}})
	})
	server := runtimeServer(t, root, mux)
	defer server.Close()
	path := filepath.Join(root, "config.yaml")
	port := strings.TrimPrefix(server.URL, "http://127.0.0.1:")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("workspace: %q\nport: %s\n", root, port)), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI("requeue", "run-1", "--config", path)
	if code != 0 || requested != 1 {
		t.Fatalf("requeue: %d %d %s", code, requested, errOut)
	}
	for _, want := range []string{"Run run-1: requeued 1 stopped publications.", "review report · round 1, attempt 1", "github.forbidden"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q: %s", want, out)
		}
	}
}

// manualSnapshot is the runtime's prepared-run answer for takeover. The CLI
// must rebuild the resume invocation locally from it.
func manualSnapshot(id, session string) workflow.Run {
	return workflow.Run{
		ID: id, Repository: "owner/repo", IssueNumber: 7,
		State: workflow.Manual, Phase: workflow.Implement, TakeoverStatus: workflow.TakeoverManual,
		RoleSettings: &config.RunRoleSettings{Implementer: config.RoleSettings{Agent: "claude", HarnessSource: "config"}},
		Implementer:  &workflow.ImplementSnapshot{Agent: "claude", SessionID: session},
	}
}

// takeoverCLIConfig writes a CLI configuration whose harness executable is a
// local script, not a server reply.
func takeoverCLIConfig(t *testing.T, root, port, executable string) string {
	t.Helper()
	path := filepath.Join(root, "config.yaml")
	configText := fmt.Sprintf("workspace: %q\nport: %s\nagents:\n  claude:\n    executable: %q\n", root, port, executable)
	if err := os.WriteFile(path, []byte(configText), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCLITakeoverResumesExactIDInWorktree(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "runs", "run-1"), 0700); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(root, "worktrees", "owner-repo", "run-1")
	if err := os.MkdirAll(worktree, 0700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "harness's executable")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s\\n' \"$PWD\" \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	id := "01a10c5b-38a9-7330-b833-04e365cb5f37"
	token := "takeover-token"
	requests := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(web.Status{Workspace: root, Token: token})
	})
	mux.HandleFunc("POST /api/runs/run-1/takeover", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "http://"+r.Host || r.Header.Get("X-CSRF-Token") != token {
			http.Error(w, "unauthorized", 403)
			return
		}
		requests++
		json.NewEncoder(w).Encode(manualSnapshot("run-1", id))
	})
	server := runtimeServer(t, root, mux)
	defer server.Close()
	path := takeoverCLIConfig(t, root, strings.TrimPrefix(server.URL, "http://127.0.0.1:"), executable)
	code, out, errOut := runCLI("takeover", "run-1", "--config", path)
	if code != 0 || requests != 1 || !strings.Contains(out, worktree+"\n--resume\n"+id+"\n--permission-mode\ndefault\n") {
		t.Fatalf("takeover launch: %d %d %q %s", code, requests, out, errOut)
	}
	if err := os.Remove(executable); err != nil {
		t.Fatal(err)
	}
	code, _, errOut = runCLI("takeover", "run-1", "--config", path)
	if code != 1 || !strings.Contains(errOut, "takeover.launch_failed") {
		t.Fatalf("launch failure missing manual recovery: %d %s", code, errOut)
	}
	// A runtime for a different workspace cannot prove this workspace's secret.
	other := t.TempDir()
	if _, err := runtimeauth.EnsureSecret(other); err != nil {
		t.Fatal(err)
	}
	otherPath := takeoverCLIConfig(t, other, strings.TrimPrefix(server.URL, "http://127.0.0.1:"), executable)
	code, _, errOut = runCLI("takeover", "run-1", "--config", otherPath)
	if code != 1 || requests != 2 || !strings.Contains(errOut, "runtime.unauthenticated") {
		t.Fatalf("takeover trusted another workspace: %d %d %s", code, requests, errOut)
	}
}

func TestCLITakeoverBuildsTheCommandLocally(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "runs", "run-1"), 0700); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(root, "worktrees", "owner-repo", "run-1")
	if err := os.MkdirAll(worktree, 0700); err != nil {
		t.Fatal(err)
	}
	configured := filepath.Join(root, "configured-harness")
	marker := filepath.Join(root, "launched")
	if err := os.WriteFile(configured, []byte("#!/bin/sh\nprintf 'ran\\n' >> '"+marker+"'\nprintf '%s\\n' \"$PWD\" \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	impostor := filepath.Join(root, "response-executable")
	if err := os.WriteFile(impostor, []byte("#!/bin/sh\nprintf 'impostor\\n' >> '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	id := "01a10c5b-38a9-7330-b833-04e365cb5f37"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(web.Status{Workspace: root, Token: "token"})
	})
	mux.HandleFunc("POST /api/runs/run-1/takeover", func(w http.ResponseWriter, r *http.Request) {
		// Even an authenticated answer that tries to look like a command cannot
		// choose the executable, arguments or directory.
		json.NewEncoder(w).Encode(struct {
			workflow.Run
			Executable string   `json:"executable"`
			Args       []string `json:"args"`
			Dir        string   `json:"dir"`
		}{manualSnapshot("run-1", id), impostor, []string{"--resume", "impostor"}, root})
	})
	server := runtimeServer(t, root, mux)
	defer server.Close()
	path := takeoverCLIConfig(t, root, strings.TrimPrefix(server.URL, "http://127.0.0.1:"), configured)
	code, out, errOut := runCLI("takeover", "run-1", "--config", path)
	if code != 0 {
		t.Fatalf("takeover: %d %s", code, errOut)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "ran\n" {
		t.Fatalf("response executable ran: %q %v", data, err)
	}
	if !strings.Contains(out, worktree+"\n--resume\n"+id+"\n--permission-mode\ndefault\n") || strings.Contains(out, "impostor") {
		t.Fatalf("takeover used a response command: %q", out)
	}
}

func TestCLITakeoverRefusesAnAnswerForAnotherRun(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "runs", "run-1"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "worktrees", "owner-repo", "run-1"), 0700); err != nil {
		t.Fatal(err)
	}
	launched := filepath.Join(root, "launched")
	executable := filepath.Join(root, "configured-harness")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf launched > '"+launched+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(web.Status{Workspace: root, Token: "token"})
	})
	mux.HandleFunc("POST /api/runs/run-1/takeover", func(w http.ResponseWriter, r *http.Request) {
		// A proven runtime that answers for another run must not choose a
		// different worktree under this run's interactive lock.
		json.NewEncoder(w).Encode(manualSnapshot("run-2", "01a10c5b-38a9-7330-b833-04e365cb5f37"))
	})
	server := runtimeServer(t, root, mux)
	defer server.Close()
	path := takeoverCLIConfig(t, root, strings.TrimPrefix(server.URL, "http://127.0.0.1:"), executable)
	code, out, errOut := runCLI("takeover", "run-1", "--config", path)
	if code != 1 || !strings.Contains(errOut, "runtime.mismatch") || strings.Contains(out, "MANUAL") {
		t.Fatalf("mismatched answer accepted: %d %q %s", code, out, errOut)
	}
	if _, err := os.Stat(launched); !os.IsNotExist(err) {
		t.Fatalf("mismatched answer launched a command: %v", err)
	}
}

func TestCLITakeoverRefusesAResponseWithoutStoredRoleSettings(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "runs", "run-1"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "worktrees", "owner-repo", "run-1"), 0700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "configured-harness")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf launched\n"), 0700); err != nil {
		t.Fatal(err)
	}
	origin := manualSnapshot("run-1", "01a10c5b-38a9-7330-b833-04e365cb5f37")
	origin.RoleSettings = nil
	origin.Implementer.Agent = ""
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(web.Status{Workspace: root, Token: "token"})
	})
	mux.HandleFunc("POST /api/runs/run-1/takeover", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(origin)
	})
	server := runtimeServer(t, root, mux)
	defer server.Close()
	path := takeoverCLIConfig(t, root, strings.TrimPrefix(server.URL, "http://127.0.0.1:"), executable)
	code, out, errOut := runCLI("takeover", "run-1", "--config", path)
	if code != 1 || !strings.Contains(errOut, "takeover.harness_unknown") || strings.Contains(out, "launched") {
		t.Fatalf("unidentified harness launched: %d %q %s", code, out, errOut)
	}
}

func TestConcurrentCLITakeoverLaunchesOneInteractiveProcess(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "runs", "run-1"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "worktrees", "owner-repo", "run-1"), 0700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "interactive")
	started, release, launches := filepath.Join(root, "started"), filepath.Join(root, "release"), filepath.Join(root, "launches")
	script := "#!/bin/sh\nprintf 'launch\\n' >> '" + launches + "'\ntouch '" + started + "'\nwhile [ ! -f '" + release + "' ]; do sleep 0.01; done\n"
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	defer os.WriteFile(release, nil, 0600)
	snapshot := manualSnapshot("run-1", "01a10c5b-38a9-7330-b833-04e365cb5f37")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(web.Status{Workspace: root, Token: "token"})
	})
	mux.HandleFunc("POST /api/runs/run-1/takeover", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(snapshot)
	})
	server := runtimeServer(t, root, mux)
	defer server.Close()
	configPath := takeoverCLIConfig(t, root, strings.TrimPrefix(server.URL, "http://127.0.0.1:"), executable)
	type result struct {
		code int
		err  string
	}
	first, second := make(chan result, 1), make(chan result, 1)
	invoke := func(done chan result) {
		code, _, err := runCLI("takeover", "run-1", "--config", configPath)
		done <- result{code, err}
	}
	go invoke(first)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first interactive process did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	go invoke(second)
	var repeated result
	returned := false
	select {
	case repeated = <-second:
		returned = true
	case <-time.After(time.Second):
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	initial := <-first
	if !returned {
		repeated = <-second
	}
	if initial.code != 0 || repeated.code != 1 || !strings.Contains(repeated.err, "takeover.interactive_running") {
		t.Fatalf("concurrent interactive resumes: %+v %+v", initial, repeated)
	}
	data, err := os.ReadFile(launches)
	if err != nil || string(data) != "launch\n" {
		t.Fatalf("duplicate interactive launch: %q %v", data, err)
	}
	// Exit releases ownership, so later intentional resume remains usable.
	code, _, errOut := runCLI("takeover", "run-1", "--config", configPath)
	if code != 0 {
		t.Fatalf("ownership survived exit: %d %s", code, errOut)
	}
}

func TestCLIOpenFollowsTheOneTimeDashboardLink(t *testing.T) {
	root := t.TempDir()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(web.Status{Workspace: root, Token: "token"})
	})
	opened := false
	mux.HandleFunc("POST /api/dashboard/session", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "http://"+r.Host || r.Header.Get("X-CSRF-Token") != "token" {
			http.Error(w, "missing protection", 403)
			return
		}
		opened = true
		json.NewEncoder(w).Encode(map[string]string{"url": "http://127.0.0.1:7331/?token=one-time"})
	})
	server := runtimeServer(t, root, mux)
	defer server.Close()
	// Force the Linux branch so the test covers the Snap-accessible handoff and
	// the HTTP-browser selection even when the host is macOS.
	useBrowserPlatform(t, "linux")
	home := t.TempDir()
	t.Setenv("HOME", home)
	tools := t.TempDir()
	launched, htmlHandler := filepath.Join(tools, "launched"), filepath.Join(tools, "html-handler")
	writeTool(t, tools, "xdg-settings", "#!/bin/sh\necho browser.desktop\n")
	writeTool(t, tools, "gio", "#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+launched+"'\n")
	writeTool(t, tools, "xdg-open", "#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+htmlHandler+"'\n")
	t.Setenv("PATH", tools)
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_DATA_DIRS", "")
	desktop := filepath.Join(dataHome, "applications", "browser.desktop")
	if err := os.MkdirAll(filepath.Dir(desktop), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(desktop, []byte("[Desktop Entry]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("workspace: %q\nport: %s\n", root, strings.TrimPrefix(server.URL, "http://127.0.0.1:"))), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI("open", "--config", path)
	if code != 0 || !opened || !strings.Contains(out, "http://127.0.0.1:7331/?token=one-time") {
		t.Fatalf("open: %d %t %q %s", code, opened, out, errOut)
	}
	args := strings.TrimSpace(waitForFile(t, launched, 3))
	if strings.Contains(args, "one-time") || strings.Contains(args, "token=") {
		t.Fatalf("launcher received the link or its token: %q", args)
	}
	fields := strings.Split(args, "\n")
	if len(fields) != 3 || fields[0] != "launch" || fields[1] != desktop {
		t.Fatalf("launcher did not select the default web browser: %q", args)
	}
	links, err := filepath.Glob(filepath.Join(home, linuxDashboardLinksDirName, dashboardHandoffPrefix+"*", dashboardHandoffFile))
	if err != nil || len(links) != 1 || links[0] != fields[2] {
		t.Fatalf("launcher did not receive the private handoff page: %q %v", links, err)
	}
	assertHandoffPage(t, links[0])
	page, err := os.ReadFile(links[0])
	if err != nil || !strings.Contains(string(page), "http://127.0.0.1:7331/?token=one-time") {
		t.Fatalf("handoff page does not redirect to the link: %q %v", page, err)
	}
	if _, err := os.Stat(htmlHandler); !os.IsNotExist(err) {
		t.Fatalf("html file association was used instead of the web browser: %v", err)
	}
}

func TestCLIHandbackDisclosesSelectionAndAdditionalRound(t *testing.T) {
	root := t.TempDir()
	token := "handback-token"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(web.Status{Workspace: root, Token: token})
	})
	mux.HandleFunc("POST /api/runs/manual/handback", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "http://"+r.Host || r.Header.Get("X-CSRF-Token") != token {
			http.Error(w, "missing protection", 403)
			return
		}
		json.NewEncoder(w).Encode(workflow.Run{ID: "manual", State: workflow.Active, Phase: workflow.Review, Handbacks: []workflow.HandbackSnapshot{{NextState: workflow.Active, NextPhase: workflow.Review, Round: 2, GrantedRound: 2}}})
	})
	server := runtimeServer(t, root, mux)
	defer server.Close()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("workspace: %q\nport: %s\n", root, strings.TrimPrefix(server.URL, "http://127.0.0.1:"))), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI("--config", path, "handback", "manual")
	if code != 0 || !strings.Contains(out, "ACTIVE/review") || !strings.Contains(out, "Additional review round granted: 2") {
		t.Fatalf("handback: %d %s %s", code, out, errOut)
	}
}

func TestCLITakeoverOwnsInteractiveLockBeforeDelayedResponse(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "runs", "run-1"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "worktrees", "owner-repo", "run-1"), 0700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "interactive")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf resumed\n"), 0700); err != nil {
		t.Fatal(err)
	}
	handbackAttempt := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(web.Status{Workspace: root, Token: "token"})
	})
	mux.HandleFunc("POST /api/runs/run-1/takeover", func(w http.ResponseWriter, r *http.Request) {
		// Manual preparation has finished, but its HTTP response has not reached
		// the CLI. Handback must be excluded throughout this launch window.
		lock, err := runner.AcquireInteractive(root, "run-1")
		if err == nil {
			lock.Close()
		}
		handbackAttempt <- err
		json.NewEncoder(w).Encode(manualSnapshot("run-1", "01a10c5b-38a9-7330-b833-04e365cb5f37"))
	})
	server := runtimeServer(t, root, mux)
	defer server.Close()
	path := takeoverCLIConfig(t, root, strings.TrimPrefix(server.URL, "http://127.0.0.1:"), executable)
	code, out, errOut := runCLI("--config", path, "takeover", "run-1")
	if code != 0 || !strings.Contains(out, "resumed") {
		t.Fatalf("takeover: %d %s %s", code, out, errOut)
	}
	if err := <-handbackAttempt; err == nil || !strings.Contains(err.Error(), "takeover.interactive_running") {
		t.Fatalf("handback could finish before delayed CLI launch: %v", err)
	}
	// A completed launch must release the same ownership reservation.
	lock, err := runner.AcquireInteractive(root, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
}

func TestCLIStatusShowsHarnessAvailabilityAndWaitKinds(t *testing.T) {
	for _, prelaunch := range []bool{true, false} {
		t.Run(map[bool]string{true: "account-gate", false: "interrupted-execution"}[prelaunch], func(t *testing.T) {
			root := t.TempDir()
			reset := time.Date(2026, 10, 8, 12, 5, 0, 0, time.UTC)
			wait := workflow.HarnessWait{Harness: "claude", Reason: "temporary_limit", Phase: workflow.Review, Round: 1, ResetAt: reset, ResetTimeSource: "reported", Source: "native-fixture"}
			if !prelaunch {
				wait.AttemptID = "interrupted-1"
				wait.Attempt = 1
				wait.Consecutive = 2
				wait.Allowance = 3
			}
			server := runtimeServer(t, root, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(web.Status{Workspace: root, Token: "runtime-token", Harnesses: []scheduler.HarnessAvailability{{Harness: "claude", Available: false, ResetAt: reset, ResetTimeSource: "reported", Source: "native-fixture"}, {Harness: "codex", Available: true}}, Runs: []workflow.Run{{ID: "waiting-run", Repository: "owner/repo", IssueNumber: 7, State: workflow.WaitingForHarness, Phase: workflow.Review, HarnessWait: &wait, HarnessWaitHistory: []workflow.HarnessWait{wait}, Retries: []workflow.RetrySnapshot{{NextState: workflow.WaitingForHarness, NextPhase: workflow.Review, Round: 1, GrantedWait: 4}}}}})
			}))
			defer server.Close()
			path := filepath.Join(root, "config.yaml")
			if err := os.WriteFile(path, []byte(fmt.Sprintf("workspace: %q\nport: %s\n", root, strings.TrimPrefix(server.URL, "http://127.0.0.1:"))), 0600); err != nil {
				t.Fatal(err)
			}
			code, out, stderr := runCLI("status", "--config", path)
			if code != 0 {
				t.Fatalf("status failed %d %s", code, stderr)
			}
			for _, text := range []string{"Harness claude: waiting until 2026-10-08T12:05:00Z (reported; native-fixture)", "Harness codex: available", "owner/repo#7  WAITING_FOR_HARNESS/review", "review / round 1 · reviewer", "temporary_limit", "Exactly one additional usage-limit wait granted; allowance 4"} {
				if !strings.Contains(out, text) {
					t.Errorf("missing status %q: %s", text, out)
				}
			}
			if prelaunch {
				if !strings.Contains(out, "Account gate; no execution started.") || strings.Contains(out, "consecutive 0 / allowance 0") {
					t.Fatalf("misleading account-gate status: %s", out)
				}
			} else if !strings.Contains(out, "consecutive 2 / allowance 3") || !strings.Contains(out, "interrupted-1") {
				t.Fatalf("interrupted-execution diagnostics missing: %s", out)
			}
		})
	}
}

func TestCLIStatusShowsCreditWaitProbeAndCapabilityGatedRetry(t *testing.T) {
	for _, eligible := range []bool{false, true} {
		t.Run(fmt.Sprint(eligible), func(t *testing.T) {
			root := t.TempDir()
			wait := workflow.HarnessWait{Harness: "claude", Reason: "credits_exhausted", Phase: workflow.Implement}
			server := runtimeServer(t, root, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(web.Status{Workspace: root, Token: "runtime-token", Harnesses: []scheduler.HarnessAvailability{{Harness: "claude", Reason: "credits_exhausted", ProbeID: "probe", ProbeRunID: "selected-run", ProbeAttemptID: "execution"}}, Runs: []workflow.Run{{ID: "waiting-run", Repository: "owner/repo", IssueNumber: 7, State: workflow.WaitingForHarness, Phase: workflow.Implement, HarnessWait: &wait, HarnessWaitHistory: []workflow.HarnessWait{wait}, RetryEligible: eligible, CreditProbes: []workflow.CreditProbe{{Harness: "claude", Status: "released", AttemptID: "old-execution"}}}}})
			}))
			defer server.Close()
			path := filepath.Join(root, "config.yaml")
			if err := os.WriteFile(path, []byte(fmt.Sprintf("workspace: %q\nport: %s\n", root, strings.TrimPrefix(server.URL, "http://127.0.0.1:"))), 0600); err != nil {
				t.Fatal(err)
			}
			code, out, stderr := runCLI("status", "--config", path)
			if code != 0 {
				t.Fatalf("status %d %s", code, stderr)
			}
			for _, value := range []string{"credits exhausted; explicit recovery required", "recovery probe probe for run selected-run", "running execution execution", "no scheduled reset", "Credit recovery probe: claude · released · execution old-execution"} {
				if !strings.Contains(out, value) {
					t.Fatalf("missing %s: %s", value, out)
				}
			}
			if strings.Contains(out, "Retry: mergeyard retry waiting-run") != eligible || strings.Contains(out, "0001-01-01") {
				t.Fatalf("credit Retry eligibility: %s", out)
			}
		})
	}
}

func TestCLIHandbackExplainsTimedAndCreditWaiting(t *testing.T) {
	for _, reason := range []string{"temporary_limit", "credits_exhausted"} {
		t.Run(reason, func(t *testing.T) {
			root := t.TempDir()
			wait := workflow.HarnessWait{Harness: "codex", Reason: reason, Phase: workflow.Review, Round: 2}
			if reason == "temporary_limit" {
				wait.ResetAt = time.Date(2026, 10, 9, 13, 0, 0, 0, time.UTC)
				wait.ResetTimeSource = "reported"
			}
			mux := http.NewServeMux()
			mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(web.Status{Workspace: root, Token: "runtime-token"})
			})
			mux.HandleFunc("POST /api/runs/manual/handback", func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(workflow.Run{ID: "manual", State: workflow.WaitingForHarness, Phase: workflow.Review, HarnessWait: &wait, Handbacks: []workflow.HandbackSnapshot{{NextState: workflow.WaitingForHarness, NextPhase: workflow.Review, Round: 2, GrantedRound: 2}}})
			})
			server := runtimeServer(t, root, mux)
			defer server.Close()
			path := filepath.Join(root, "config.yaml")
			if err := os.WriteFile(path, []byte(fmt.Sprintf("workspace: %q\nport: %s\n", root, strings.TrimPrefix(server.URL, "http://127.0.0.1:"))), 0600); err != nil {
				t.Fatal(err)
			}
			code, out, stderr := runCLI("--config", path, "handback", "manual")
			if code != 0 {
				t.Fatalf("handback %d %s", code, stderr)
			}
			expected := "automatic resumption"
			if reason == "credits_exhausted" {
				expected = "explicit recovery required"
			}
			for _, value := range []string{"WAITING_FOR_HARNESS/review", "Additional review round granted: 2", expected, "Manual conversation success does not clear the harness block"} {
				if !strings.Contains(out, value) {
					t.Fatalf("missing %q: %s", value, out)
				}
			}
		})
	}
}

func TestCLIHarnessCheckUsesRuntimeProtections(t *testing.T) {
	root := t.TempDir()
	workspaceIdentity := root
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(web.Status{Workspace: workspaceIdentity, Token: "runtime-token"})
	})
	mux.HandleFunc("POST /api/harnesses/{harness}/check", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "http://"+r.Host || r.Header.Get("X-CSRF-Token") != "runtime-token" {
			http.Error(w, "missing protection", 403)
			return
		}
		calls++
		json.NewEncoder(w).Encode(workflow.HarnessCheck{ID: "check-id", Harness: r.PathValue("harness"), Status: "running"})
	})
	server := runtimeServer(t, root, mux)
	defer server.Close()
	path := filepath.Join(root, "config.yaml")
	port := strings.TrimPrefix(server.URL, "http://127.0.0.1:")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("workspace: %q\nport: %s\n", root, port)), 0600); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"claude", "codex"} {
		code, out, errOut := runCLI("harness", "check", agent, "--config", path)
		if code != 0 || !strings.Contains(out, "uses account quota") || !strings.Contains(out, "no engineering work") || !strings.Contains(out, "check-id") {
			t.Fatalf("check %s: %d %s %s", agent, code, out, errOut)
		}
	}
	code, _, errOut := runCLI("harness", "check", "../codex", "--config", path)
	if code != 1 || calls != 2 || !strings.Contains(errOut, "requires claude or codex") {
		t.Fatalf("invalid harness: %d %d %s", code, calls, errOut)
	}
	workspaceIdentity = t.TempDir()
	code, _, errOut = runCLI("harness", "check", "claude", "--config", path)
	if code != 1 || calls != 2 || !strings.Contains(errOut, "workspace.mismatch") {
		t.Fatalf("workspace bypass: %d %d %s", code, calls, errOut)
	}
}

func TestCLIRetryGuidanceAndStatus(t *testing.T) {
	root := t.TempDir()
	note := "Do it this way; $(touch injected)\n`id` --config --help"
	received := ""
	snapshot := workflow.Run{ID: "guided", Repository: "owner/repo", IssueNumber: 7, State: workflow.Active, Phase: workflow.Implement, Retries: []workflow.RetrySnapshot{{Guidance: &workflow.HumanGuidance{Author: "user", CreatedAt: "2026-10-12T12:00:00Z", Text: note}, NextState: workflow.Active, NextPhase: workflow.Implement}}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(web.Status{Workspace: root, Token: "token", Runs: []workflow.Run{snapshot}})
	})
	mux.HandleFunc("POST /api/runs/guided/retry", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "http://"+r.Host || r.Header.Get("X-CSRF-Token") != "token" {
			http.Error(w, "missing protection", 403)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		received = r.PostForm.Get("note")
		json.NewEncoder(w).Encode(snapshot)
	})
	server := runtimeServer(t, root, mux)
	defer server.Close()
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte(fmt.Sprintf("workspace: %q\nport: %s\n", root, strings.TrimPrefix(server.URL, "http://127.0.0.1:"))), 0600); err != nil {
		t.Fatal(err)
	}
	notePath := filepath.Join(root, "guidance.md")
	if err := os.WriteFile(notePath, []byte(note), 0600); err != nil {
		t.Fatal(err)
	}
	for _, flags := range [][]string{{"--note", note}, {"--note=" + note}, {"--note-file", notePath}} {
		args := append([]string{"retry", "guided", "--config", configPath}, flags...)
		code, out, errOut := runCLI(args...)
		if code != 0 || received != note || !strings.Contains(out, "Human guidance by user · 2026-10-12T12:00:00Z") || !strings.Contains(out, terminalText(note)) {
			t.Fatalf("retry: %d note=%q out=%s err=%s", code, received, out, errOut)
		}
	}
	code, out, errOut := runCLI("status", "--config", configPath)
	if code != 0 || !strings.Contains(out, terminalText(note)) {
		t.Fatalf("status: %d %s %s", code, out, errOut)
	}
}

func TestCLIRetryRejectsInvalidGuidanceOptionsBeforeConnecting(t *testing.T) {
	for _, args := range [][]string{
		{"retry", "guided", "--note"},
		{"retry", "guided", "--note-file"},
		{"retry", "guided", "--note", "one", "--note-file", "two"},
		{"retry", "guided", "--note", "one", "--note", "two"},
		{"status", "--note", "one"},
	} {
		code, _, stderr := runCLI(args...)
		if code != 2 || strings.Contains(stderr, "runtime_unavailable") {
			t.Fatalf("invalid options reached runtime: %v %d %s", args, code, stderr)
		}
	}
	code, _, stderr := runCLI("retry", "guided", "--note-file", filepath.Join(t.TempDir(), "missing"))
	if code != 1 || !strings.Contains(stderr, "read note file") {
		t.Fatalf("missing note file: %d %s", code, stderr)
	}
}
