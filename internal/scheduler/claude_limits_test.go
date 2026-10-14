package scheduler_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestClaudeWeeklyLimitBindingWaitsAndRecovers(t *testing.T) {
	for _, tc := range []struct {
		name           string
		phase          workflow.Phase
		cooldown       bool
		missingSession bool
	}{
		{"implement", workflow.Implement, false, false},
		{"review", workflow.Review, false, false},
		{"fix", workflow.Fix, false, false},
		{"configured cooldown", workflow.Implement, true, false},
		{"missing session recovery", workflow.Implement, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture, err := os.ReadFile("../../docs/research/claude-m3/fixtures/weekly-rejected/events.jsonl")
			if err != nil {
				t.Fatal(err)
			}
			if tc.cooldown {
				fixture = []byte(strings.ReplaceAll(string(fixture), `"resetsAt":1791972000`, `"unusedReset":1791972000`))
			}
			fixturePath := filepath.Join(t.TempDir(), "weekly-rejected.jsonl")
			if err := os.WriteFile(fixturePath, fixture, 0600); err != nil {
				t.Fatal(err)
			}
			firstAttempt := string(tc.phase) + "-1-1"
			if tc.phase == workflow.Implement {
				firstAttempt = "implement-0-1"
			}
			script := `case "$*" in *` + firstAttempt + `*)
printf partial > partial.txt
sed 's/46f8963c-6b42-491e-9924-85fad092ecad/'"$3"'/g' '` + fixturePath + `'
exit 1;; esac
`
			if tc.missingSession {
				script += `case "$*" in *implement-0-2*) printf 'No conversation found with session ID: %s\n' "$3" >&2; exit 1;; esac
`
			}
			if tc.phase == workflow.Fix {
				script += loopScript(disputedReport)
			} else {
				script += reviewScript(approvedReview)
			}
			_, runtime, api, _, cfg, r := pairingFlow(t, "claude", "claude", disputedReport)
			replacePhaseScript(t, cfg, tc.phase, script)
			cfg.UsageLimits.Cooldown = 7 * time.Minute
			now := time.Date(2026, 10, 13, 12, 2, 5, 0, time.UTC)
			deps := scheduler.Dependencies{GitHub: branchGitHub{api}, Runner: r, Now: func() time.Time { return now }}
			s, err := scheduler.New(cfg, schedulerResources(runtime), deps)
			if err != nil {
				t.Fatal(err)
			}
			waiting := finish(t, s, workflow.WaitingForHarness, tc.phase)
			wantReset, wantSource := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC), "reported"
			wantSignal := "rate_limit_info.resetsAt"
			if tc.cooldown {
				wantReset, wantSource, wantSignal = now.Add(7*time.Minute), "default_cooldown", "rate_limit_info.status"
			}
			if waiting.HarnessWait == nil || !waiting.HarnessWait.ResetAt.Equal(wantReset) || waiting.HarnessWait.ResetTimeSource != wantSource || waiting.HarnessWait.Source != wantSignal || len(waiting.HarnessWaitHistory) != 1 {
				t.Fatalf("native limit did not persist expected waiting: %+v", waiting)
			}
			availability, err := s.HarnessAvailability(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, harness := range availability {
				if harness.Harness == "claude" && (harness.Available || !harness.TemporaryLimitDetection || harness.CreditExhaustionDetection || !harness.ResetAt.Equal(wantReset)) {
					t.Fatalf("native limit did not gate Claude: %+v", harness)
				}
			}
			session := waiting.Implementer.SessionID
			if tc.phase == workflow.Review {
				session = waiting.Review.SessionID
			}
			if tc.phase != workflow.Review {
				paths, err := filepath.Glob(filepath.Join(runtime.Workspace.Root, "worktrees", "*", "*"))
				if err != nil || len(paths) != 1 {
					t.Fatalf("managed worktrees: %v %v", paths, err)
				}
				if data, err := os.ReadFile(filepath.Join(paths[0], "partial.txt")); err != nil || string(data) != "partial" {
					t.Fatalf("partial work lost: %s %v", data, err)
				}
			}
			// Recreate the scheduler against the persisted wait before reset.
			s, err = scheduler.New(cfg, schedulerResources(runtime), deps)
			if err != nil {
				t.Fatal(err)
			}
			for range 3 {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			runs, err := s.Runs(context.Background())
			if err != nil || len(runs) != 1 || runs[0].State != workflow.WaitingForHarness || runs[0].HarnessWait.AttemptID != waiting.HarnessWait.AttemptID {
				t.Fatalf("restart bypassed reset: %+v %v", runs, err)
			}
			now = wantReset
			done := finish(t, s, workflow.WaitingForCI, workflow.Review)
			attempt, resumedSession := done.Implementer.Attempt, done.Implementer.SessionID
			if tc.phase == workflow.Review {
				attempt, resumedSession = done.Review.Attempt, done.Review.SessionID
			} else if tc.phase == workflow.Fix {
				attempt, resumedSession = done.Fix.Attempt, done.Fix.SessionID
			}
			wantAttempt := 2
			if tc.missingSession {
				wantAttempt = 3
				if len(done.SessionRecoveries) != 1 || done.SessionRecoveries[0].PreviousSessionID != session || resumedSession == session {
					t.Fatalf("missing-session recovery failed: %+v", done)
				}
			} else if resumedSession != session {
				t.Fatalf("same-session resume lost identity: %+v", done)
			}
			if attempt != wantAttempt || len(done.HarnessWaitHistory) != 1 || api.creations != 1 {
				t.Fatalf("limit spent ordinary attempt budget or duplicated publication: %+v", done)
			}
		})
	}
}
