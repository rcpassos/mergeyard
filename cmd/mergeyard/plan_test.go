package main

import (
	"context"
	"encoding/json"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/web"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/app"
)

func TestPlanOfflineReadsOnly(t *testing.T) {
	for _, persisted := range []bool{false, true} {
		t.Run(strconv.FormatBool(persisted), func(t *testing.T) {
			root := t.TempDir()
			workspace := filepath.Join(root, "workspace")
			database := filepath.Join(workspace, "state.db")
			var before []byte
			if persisted {
				owner, err := app.Open(context.Background(), workspace)
				if err != nil {
					t.Fatal(err)
				}
				if err := owner.Close(); err != nil {
					t.Fatal(err)
				}
				before, err = os.ReadFile(database)
				if err != nil {
					t.Fatal(err)
				}
			}
			tools := filepath.Join(root, "tools")
			if err := os.Mkdir(tools, 0700); err != nil {
				t.Fatal(err)
			}
			script := `#!/bin/sh
case "$*" in
 *"--method GET"*"repos/owner/repo/issues?state=open&per_page=100"*) printf '%s\n' '[{"number":7,"title":"Running","state":"open","created_at":"2026-10-01T00:00:00Z","labels":[{"name":"ready-for-agent"},{"name":"agent-running"}]}]' ;;
 *) echo "unexpected GitHub write or read: $*" >&2; exit 1 ;;
esac
`
			if err := os.WriteFile(filepath.Join(tools, "gh"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := listener.Addr().(*net.TCPAddr).Port
			listener.Close()
			cfg := filepath.Join(root, "config.yaml")
			if err := os.WriteFile(cfg, []byte("port: "+strconv.Itoa(port)+"\nworkspace: "+strconv.Quote(workspace)+"\nrepositories:\n  - repo: owner/repo\n"), 0600); err != nil {
				t.Fatal(err)
			}
			code, out, stderr := runCLI("plan", "--config", cfg, "--json")
			if code != 0 || stderr != "" {
				t.Fatalf("plan: code=%d out=%s err=%s", code, out, stderr)
			}
			var report struct {
				Issues []struct{ Decision, Reason string }
			}
			if err := json.Unmarshal([]byte(out), &report); err != nil {
				t.Fatal(err)
			}
			if len(report.Issues) != 1 || report.Issues[0].Decision != "skipped" || report.Issues[0].Reason != "running_label" {
				t.Fatalf("plan: %s", out)
			}
			if persisted {
				after, err := os.ReadFile(database)
				if err != nil || string(after) != string(before) {
					t.Fatalf("database changed: %v", err)
				}
			} else if _, err := os.Stat(workspace); !os.IsNotExist(err) {
				t.Fatalf("workspace created: %v", err)
			}
			code, out, stderr = runCLI("plan", "--config", cfg)
			if code != 0 || stderr != "" || !strings.Contains(out, "owner/repo#7 skipped: running_label") {
				t.Fatalf("text plan: %d %s %s", code, out, stderr)
			}
		})
	}
}

func TestPlanUsesOwningControlPlane(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "never-created")
	var requests []string
	report := scheduler.Plan{SchemaVersion: 1, Paused: true, Issues: []scheduler.PlanIssue{{Repository: "owner/runtime", Number: 8, Title: "Runtime settings", Decision: "would_claim", RoleSettings: &config.RunRoleSettings{Implementer: config.RoleSettings{Agent: "codex", HarnessSource: "label", Skills: []string{"implement"}}, Reviewer: config.RoleSettings{Agent: "claude", HarnessSource: "config", Model: "runtime-model", Skills: []string{}}}}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.String())
		if r.Method != "GET" {
			t.Error("plan wrote to control plane")
		}
		switch r.URL.Path {
		case "/api/status":
			json.NewEncoder(w).Encode(web.Status{Workspace: workspace})
		case "/api/plan":
			json.NewEncoder(w).Encode(report)
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte("port: "+strconv.Itoa(port)+"\nworkspace: "+strconv.Quote(workspace)+"\nrepositories: [{repo: owner/local}]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := runCLI("plan", "--config", path, "--repo", "owner/runtime", "--json")
	encoded, _ := json.Marshal(report)
	if code != 0 || stderr != "" || out != string(encoded)+"\n" {
		t.Fatalf("online plan=%d %s %s", code, out, stderr)
	}
	if strings.Join(requests, ",") != "GET /api/status,GET /api/plan?repo=owner%2Fruntime" {
		t.Fatalf("requests=%v", requests)
	}
	code, out, stderr = runCLI("plan", "--config", path)
	if code != 0 || stderr != "" || !strings.Contains(out, "source label") || !strings.Contains(out, "runtime-model") {
		t.Fatalf("text=%d %s %s", code, out, stderr)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatal("online plan created workspace")
	}
}

func TestPlanFlagsRequirePlan(t *testing.T) {
	for _, args := range [][]string{{"status", "--json"}, {"start", "--repo", "owner/repo"}, {"plan", "--repo"}, {"plan", "--json=true"}, {"plan", "extra"}} {
		code, _, _ := runCLI(args...)
		if code != 2 {
			t.Fatalf("args=%v code=%d", args, code)
		}
	}
}

func TestPlanOnlineReturnsEveryIssueInLargeQueue(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "never-created")
	report := scheduler.Plan{SchemaVersion: 1, Paused: true, Issues: make([]scheduler.PlanIssue, 13000)}
	for i := range report.Issues {
		report.Issues[i] = scheduler.PlanIssue{Repository: "owner/repo", Number: i + 1, Title: strings.Repeat("x", 240), Decision: "waiting", Reason: "scheduler_paused"}
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) <= 4<<20 {
		t.Fatalf("fixture must exceed the former response cap: %d bytes", len(encoded))
	}
	t.Logf("%d issues; encoded response: %d bytes", len(report.Issues), len(encoded)+1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("plan wrote to control plane: %s", r.Method)
		}
		switch r.URL.Path {
		case "/api/status":
			json.NewEncoder(w).Encode(web.Status{Workspace: workspace})
		case "/api/plan":
			w.Write(append(encoded, '\n'))
		default:
			t.Errorf("unexpected request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	port := server.Listener.Addr().(*net.TCPAddr).Port
	if err := os.WriteFile(path, []byte("port: "+strconv.Itoa(port)+"\nworkspace: "+strconv.Quote(workspace)+"\nrepositories: [{repo: owner/repo}]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, asJSON := range []bool{true, false} {
		t.Run(strconv.FormatBool(asJSON), func(t *testing.T) {
			args := []string{"plan", "--config", path}
			if asJSON {
				args = append(args, "--json")
			}
			code, out, stderr := runCLI(args...)
			if code != 0 || stderr != "" {
				t.Fatalf("large online plan failed: code=%d stderr=%s", code, stderr)
			}
			if asJSON {
				var got scheduler.Plan
				if err := json.Unmarshal([]byte(out), &got); err != nil {
					t.Fatal(err)
				}
				if len(got.Issues) != 13000 {
					t.Fatalf("queue truncated: got %d issues", len(got.Issues))
				}
				for i, issue := range got.Issues {
					if issue.Number != i+1 || issue.Repository != "owner/repo" || issue.Title != strings.Repeat("x", 240) || issue.Decision != "waiting" || issue.Reason != "scheduler_paused" {
						t.Fatalf("incomplete issue %d: %+v", i+1, issue)
					}
				}
			} else {
				if strings.Count(out, " waiting: scheduler_paused") != 13000 || !strings.Contains(out, "owner/repo#13000 waiting: scheduler_paused") {
					t.Fatal("text output omitted queue decisions")
				}
			}
		})
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatal("online plan created workspace")
	}
}
