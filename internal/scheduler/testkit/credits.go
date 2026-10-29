package testkit

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type CreditHarness struct {
	*ClassifiedHarness
	Detection bool
}

func (h *CreditHarness) Capabilities() harness.HarnessCapabilities {
	c := h.HarnessAdapter.Capabilities()
	c.CreditExhaustionDetection = h.Detection
	return c
}

func OlderProbeCompletion(t *testing.T, kind harness.FailureKind) {
	marker := filepath.Join(t.TempDir(), "new-restriction")
	proof := filepath.Join(t.TempDir(), "probe-completion")
	script := `case "$(git branch --show-current)" in mergeyard/issue-7) exit 1;; *) while [ ! -f '` + marker + `' ]; do sleep 0.05; done; exit 1;; esac`
	_, rt, api, _, cfg, r := TmuxFlow(t, script)
	cfg.Concurrency = 2
	cfg.Repositories[0].Concurrency = 2
	api.Issues["owner/repo"] = append(api.Issues["owner/repo"], Ready(8))
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	reset := now.Add(time.Hour)
	h := &CreditHarness{ClassifiedHarness: &ClassifiedHarness{HarnessAdapter: harness.NewClaude(cfg.Agents.Claude), Outcomes: []harness.FailureClassification{{Kind: harness.CreditsExhausted}, {Kind: kind, ResetAt: reset}}}, Detection: true}
	deps := scheduler.Dependencies{Now: func() time.Time { return now }, GitHub: api, Runner: r, Harnesses: map[string]harness.HarnessAdapter{"claude": h}}
	s, err := TestScheduler(cfg, SchedulerResources(rt), deps)
	if err != nil {
		t.Fatal(err)
	}
	var selected workflow.Run
	WaitFor(t, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range runs {
			if run.IssueNumber == 7 && run.State == workflow.NeedsAttention {
				selected = run
				return true
			}
		}
		return false
	})
	ReplacePhaseScript(t, cfg, workflow.Implement, `while [ ! -f '`+proof+`' ]; do sleep 0.05; done
`+SuccessfulScript)
	if _, err := s.Retry(context.Background(), selected.ID); err != nil {
		t.Fatal(err)
	}
	WaitFor(t, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		states, err := s.HarnessAvailability(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range states {
			if v.Harness == "claude" {
				return v.ProbeAttemptID != ""
			}
		}
		return false
	})
	if err := os.WriteFile(marker, []byte("release"), 0600); err != nil {
		t.Fatal(err)
	}
	WaitFor(t, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range runs {
			if run.IssueNumber == 8 {
				if kind == harness.TemporaryLimit {
					return run.State == workflow.WaitingForHarness
				}
				return run.State == workflow.NeedsAttention
			}
		}
		return false
	})
	runs, err := s.Runs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if run.IssueNumber == 8 {
			if _, err := s.Retry(context.Background(), run.ID); err == nil {
				t.Fatal("new restriction allowed another probe while older execution still runs")
			}
		}
	}
	if err := os.WriteFile(proof, []byte("complete"), 0600); err != nil {
		t.Fatal(err)
	}
	WaitFor(t, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		run, err := rt.Workflow.Get(context.Background(), selected.ID)
		if err != nil {
			t.Fatal(err)
		}
		return run.Phase == workflow.Review
	})
	s, err = TestScheduler(cfg, SchedulerResources(rt), deps)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := s.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	states, err := s.HarnessAvailability(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range states {
		if v.Harness == "claude" && (v.Available || v.ProbeID != "" || v.Reason != string(kind) || (kind == harness.TemporaryLimit && !v.ResetAt.Equal(reset))) {
			t.Fatalf("old proof cleared newer restriction: %+v", v)
		}
	}
	if kind == harness.TemporaryLimit {
		recovered, err := rt.Workflow.Get(context.Background(), selected.ID)
		if err != nil || recovered.CreditProbes[0].Status != "recovered" {
			t.Fatalf("credit proof lost alongside timed gate: %+v %v", recovered, err)
		}
		now = reset
		states, err = s.HarnessAvailability(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range states {
			if v.Harness == "claude" && !v.Available {
				t.Fatalf("remaining timed limit did not expire: %+v", v)
			}
		}
	}
	history, err := rt.Events.History(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range history {
		if event.Type == "harness.available" {
			t.Fatal("stale availability event")
		}
	}
}
