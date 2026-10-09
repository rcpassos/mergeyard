package scheduler_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestHarnessCheckPreparationFailureReleasesProbe(t *testing.T) {
	_, rt, api, _, cfg, r := localFlow(t, `exit 1`)
	h := &creditHarness{classifiedHarness: &classifiedHarness{HarnessAdapter: harness.NewClaude(cfg.Agents.Claude), outcome: harness.FailureClassification{Kind: harness.CreditsExhausted}}, detection: true}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{"claude": h}})
	if err != nil {
		t.Fatal(err)
	}
	finish(t, s, workflow.NeedsAttention, workflow.Implement)
	root := filepath.Join(rt.Workspace.Root, "harness-checks")
	if err := os.WriteFile(root, []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckHarness(context.Background(), "claude"); err == nil {
		t.Fatal("expected preparation error")
	}
	states, err := s.HarnessAvailability(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range states {
		if v.Harness == "claude" && (v.Available || v.ProbeID != "" || !v.CheckEligible || v.Check == nil || v.Check.Status != "released" || !strings.Contains(v.Check.Result, "directory")) {
			t.Fatalf("failed preparation retained probe: %+v", v)
		}
	}
	for range 2 {
		if _, err := s.Reconcile(context.Background()); err != nil {
			t.Fatalf("failed check blocked reconciliation: %v", err)
		}
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckHarness(context.Background(), "claude"); err != nil {
		t.Fatalf("could not select a new check: %v", err)
	}
}

type checkFailureRunner struct {
	runner.Runner
	output    string
	ambiguous bool
}

func (r checkFailureRunner) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if strings.Contains(path, "harness-checks") && filepath.Base(path) == r.output {
		return nil, errors.New("captured output unavailable")
	}
	return r.Runner.ReadFile(ctx, path)
}
func (r checkFailureRunner) SessionStatus(ctx context.Context, ref runner.SessionRef) (runner.SessionStatus, error) {
	if r.ambiguous && strings.Contains(ref.PhaseDir, "harness-checks") {
		return runner.SessionStatus{}, errors.New("process identity unavailable")
	}
	return r.Runner.SessionStatus(ctx, ref)
}

func TestHarnessCheckFailureDoesNotBlockOtherRuns(t *testing.T) {
	for _, failure := range []string{"events.jsonl", "stderr.log", "ambiguous"} {
		t.Run(failure, func(t *testing.T) {
			_, rt, api, remote, cfg, r := localFlow(t, `exit 1`)
			gitCommand(t, t.TempDir(), "config", "--file", os.Getenv("GIT_CONFIG_GLOBAL"), "--add", "url."+remote+".insteadOf", "https://github.com/owner/other.git")
			cfg.Agents.Codex.Executable = filepath.Join(t.TempDir(), "fake-codex")
			if err := os.WriteFile(cfg.Agents.Codex.Executable, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
				t.Fatal(err)
			}
			other := cfg.Repositories[0]
			other.Repo = "owner/other"
			other.Implementer.Agent = "codex"
			cfg.Repositories = append(cfg.Repositories, other)
			api.issues[other.Repo] = append(api.issues[other.Repo], ready(8))
			h := &creditHarness{classifiedHarness: &classifiedHarness{HarnessAdapter: harness.NewClaude(cfg.Agents.Claude), outcome: harness.FailureClassification{Kind: harness.CreditsExhausted}}, detection: true}
			deps := scheduler.Dependencies{GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{"claude": h}}
			s, err := scheduler.New(cfg, schedulerResources(rt), deps)
			if err != nil {
				t.Fatal(err)
			}
			// Reconcile alone never discovers the second repository's ready issue.
			cfg.Repositories[1].Enabled = false
			initial, err := scheduler.New(cfg, schedulerResources(rt), deps)
			if err != nil {
				t.Fatal(err)
			}
			run := finish(t, initial, workflow.NeedsAttention, workflow.Implement)
			if err := os.WriteFile(cfg.Agents.Claude.Executable, []byte("#!/bin/sh\nprintf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"OK\"}'"), 0700); err != nil {
				t.Fatal(err)
			}
			check, err := s.CheckHarness(context.Background(), "claude")
			if err != nil {
				t.Fatal(err)
			}
			ref := runner.SessionRef{Name: check.ProcessSession, PhaseDir: check.PhaseDir}
			waitFor(t, func() bool {
				status, err := r.SessionStatus(context.Background(), ref)
				if err != nil {
					t.Fatal(err)
				}
				return status.State == runner.SessionExited
			})
			if _, err := rt.Workflow.Transition(context.Background(), "other-run", workflow.Request{Trigger: workflow.IssueClaimed, Repository: other.Repo, IssueNumber: 8}); err != nil {
				t.Fatal(err)
			}
			deps.Runner = checkFailureRunner{Runner: r, output: failure, ambiguous: failure == "ambiguous"}
			s, err = scheduler.New(cfg, schedulerResources(rt), deps)
			if err != nil {
				t.Fatal(err)
			}
			s.Reconcile(context.Background()) // A check error may be reported, but other runs must advance.
			unrelated, err := rt.Workflow.Get(context.Background(), "other-run")
			if err != nil || unrelated.State != workflow.Active || unrelated.Phase != workflow.Implement {
				t.Fatalf("check failure blocked another harness: %+v %v", unrelated, err)
			}
			states, err := s.HarnessAvailability(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range states {
				if v.Harness == "claude" {
					if v.Available || v.Check == nil {
						t.Fatalf("failure cleared restriction: %+v", v)
					}
					if failure == "ambiguous" {
						if v.ProbeID != check.ID || v.Check.Status != "running" || v.Check.Result == "" {
							t.Fatalf("ambiguous process lost ownership: %+v", v)
						}
					} else if v.ProbeID != "" || v.Check.Status != "released" || !strings.Contains(v.Check.Result, "output") {
						t.Fatalf("ended execution retained ownership: %+v", v)
					}
				}
			}
			saved, err := rt.Workflow.Get(context.Background(), run.ID)
			if err != nil || saved.State != workflow.NeedsAttention {
				t.Fatalf("affected run changed: %+v %v", saved, err)
			}
		})
	}
}

func TestHarnessCheckConfigurationInspectionCannotProveAvailability(t *testing.T) {
	for _, mode := range []string{"invalid", "failed", "configured"} {
		t.Run(mode, func(t *testing.T) {
			_, rt, api, _, cfg, r := pairingFlow(t, "codex", "codex", disputedReport)
			replacePhaseScript(t, cfg, workflow.Implement, `exit 1`)
			h := &creditHarness{classifiedHarness: &classifiedHarness{HarnessAdapter: harness.NewCodex(cfg.Agents.Codex), outcome: harness.FailureClassification{Kind: harness.CreditsExhausted}}, detection: true}
			s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{"codex": h}})
			if err != nil {
				t.Fatal(err)
			}
			finish(t, s, workflow.NeedsAttention, workflow.Implement)
			calls := filepath.Join(t.TempDir(), "model-calls")
			response := `printf '%s\n' '{"type":"turn.completed"}'`
			if mode == "failed" {
				response = `exit 1`
			}
			if mode == "configured" {
				response = `printf '%s\n' '[{"name":"direct","enabled":true},{"name":"name.with.dot","enabled":true}]'`
			}
			script := `case "$*" in *"mcp list --json"*) ` + response + `; exit 0;; esac
case "$*" in *'mcp_servers={"direct"={enabled=false},"name.with.dot"={enabled=false}}'*) ;; *) exit 12;; esac
printf 'model\n' >> '` + calls + `'
printf '%s\n' '{"type":"turn.completed"}'`
			if err := os.WriteFile(cfg.Agents.Codex.Executable, []byte("#!/bin/sh\n"+script), 0700); err != nil {
				t.Fatal(err)
			}
			_, err = s.CheckHarness(context.Background(), "codex")
			if mode == "configured" {
				if err != nil {
					t.Fatal(err)
				}
				waitFor(t, func() bool {
					if _, err := s.Reconcile(context.Background()); err != nil {
						t.Fatal(err)
					}
					states, _ := s.HarnessAvailability(context.Background())
					for _, v := range states {
						if v.Harness == "codex" {
							return v.Available
						}
					}
					return false
				})
				data, err := os.ReadFile(calls)
				if err != nil || string(data) != "model\n" {
					t.Fatalf("minimal request count: %s %v", data, err)
				}
			} else {
				if err == nil {
					t.Fatal("configuration failure accepted")
				}
				if _, err := os.Stat(calls); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("inspection failure launched a model request")
				}
				states, err := s.HarnessAvailability(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				for _, v := range states {
					if v.Harness == "codex" && (v.Available || v.ProbeID != "" || v.Check == nil || v.Check.Status != "released" || v.Check.Result == "") {
						t.Fatalf("inspection proved availability or retained ownership: %+v", v)
					}
				}
			}
		})
	}
}
