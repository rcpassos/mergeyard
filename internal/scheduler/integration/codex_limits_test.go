package integration_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestCodexCapturedLimitWaitsAndRecovers(t *testing.T) {

	zone, err := time.LoadLocation("Europe/Lisbon")
	if err != nil {
		t.Fatal(err)
	}
	previous := time.Local
	time.Local = zone
	t.Cleanup(func() { time.Local = previous })
	for _, tc := range []struct {
		name                                        string
		stale, missingIdentity, replay, resumeLimit bool
	}{
		{name: "reported reset"}, {name: "limit on resume", resumeLimit: true}, {name: "configured cooldown", stale: true}, {name: "replayed next day", replay: true}, {name: "missing identity", missingIdentity: true},
	} {
		t.Run(tc.name, func(t *testing.T) {

			fixture, err := os.ReadFile(testkit.RepositoryPath("docs/research/codex-m3/fixtures/usage-limit-first-request/events.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			fixture = []byte(strings.ReplaceAll(string(fixture), "01a144aa-e286-7420-b07e-1fdbe4dca033", testkit.CodexID))
			if tc.missingIdentity {
				fixture = fixture[strings.IndexByte(string(fixture), '\n')+1:]
			}
			path := filepath.Join(t.TempDir(), "limit.jsonl")
			if err := os.WriteFile(path, fixture, 0600); err != nil {
				t.Fatal(err)
			}
			script := `case "$*" in *implement-0-1*) printf partial > partial.txt; cat '` + path + `'; exit 1;; esac
case "$*" in *"resume -- ` + testkit.CodexID + ` "*) ;; *) exit 9;; esac
` + testkit.CodexImplementation
			if tc.resumeLimit {
				script = strings.Replace(script, "\n"+testkit.CodexImplementation, "\n"+`case "$*" in *implement-0-2*) cat '`+path+`'; exit 1;; esac`+"\n"+testkit.CodexImplementation, 1)
			}
			_, runtime, api, _, cfg, r := testkit.TmuxCodexFlow(t, script)
			cfg.UsageLimits.Cooldown = 7 * time.Minute
			now := time.Date(2026, 10, 16, 12, 23, 36, 0, time.UTC)
			if tc.stale {
				now = now.Add(2 * time.Hour)
			}
			deps := scheduler.Dependencies{GitHub: api, Runner: r, Now: func() time.Time { return now }}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), deps)
			if err != nil {
				t.Fatal(err)
			}
			if tc.replay {
				// Launch at the capture time, then leave reconciliation until the
				// next local day, as after an offline finish and restart.
				testkit.WaitFor(t, func() bool {
					if err := s.Tick(context.Background()); err != nil {
						t.Fatal(err)
					}
					runs, err := s.Runs(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					return len(runs) == 1 && runs[0].Implementer != nil && runs[0].Implementer.Status == "running"
				})
				now = time.Date(2026, 10, 17, 0, 1, 0, 0, zone)
				s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), deps)
				if err != nil {
					t.Fatal(err)
				}
			}
			waiting := testkit.Finish(t, s, workflow.WaitingForHarness, workflow.Implement)
			want, source := time.Date(2026, 10, 16, 12, 53, 0, 0, time.UTC), "reported"
			if tc.stale || tc.replay {
				want, source = now.Add(7*time.Minute), "default_cooldown"
			}
			if waiting.HarnessWait == nil || !waiting.HarnessWait.ResetAt.Equal(want) || waiting.HarnessWait.ResetTimeSource != source || waiting.Implementer.Attempt != 1 {
				t.Fatalf("wait = %+v", waiting)
			}
			session := testkit.CodexID
			if tc.missingIdentity {
				session = ""
			}
			if waiting.Implementer.SessionID != session {
				t.Fatalf("identity = %+v", waiting.Implementer)
			}
			paths, err := filepath.Glob(filepath.Join(runtime.Workspace.Root, "worktrees", "*", "*"))
			if err != nil || len(paths) != 1 {
				t.Fatalf("worktrees %v: %v", paths, err)
			}
			if data, err := os.ReadFile(filepath.Join(paths[0], "partial.txt")); err != nil || string(data) != "partial" {
				t.Fatalf("lost work %q: %v", data, err)
			}
			s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(runtime), deps)
			if err != nil {
				t.Fatal(err)
			}
			for range 3 {
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			runs, err := s.Runs(context.Background())
			if err != nil || len(runs) != 1 || runs[0].State != workflow.WaitingForHarness || runs[0].Implementer.Attempt != 1 {
				t.Fatalf("restart bypassed wait: %+v %v", runs, err)
			}
			now = want
			if tc.missingIdentity {
				attention := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
				if attention.LastErrorCode != "harness.session_missing" || attention.Implementer.SessionID != "" || attention.Implementer.Attempt != 1 || api.Creations != 0 {
					t.Fatalf("guessed identity: %+v", attention)
				}
				return
			}
			wantAttempt, wantWaits := 2, 1
			if tc.resumeLimit {
				second := testkit.Finish(t, s, workflow.WaitingForHarness, workflow.Implement)
				if second.Implementer.Attempt != 2 || second.Implementer.SessionID != testkit.CodexID || second.HarnessWait.Consecutive != 2 {
					t.Fatalf("resumed limit = %+v", second)
				}
				now = second.HarnessWait.ResetAt
				wantAttempt, wantWaits = 3, 2
			}
			done := testkit.Finish(t, s, workflow.Active, workflow.Review)
			if done.Implementer.Attempt != wantAttempt || done.Implementer.SessionID != testkit.CodexID || len(done.HarnessWaitHistory) != wantWaits || api.Creations != 1 {
				t.Fatalf("resume = %+v", done)
			}
		})
	}
}
