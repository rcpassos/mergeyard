package integration_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestReviewerReceivesPassedLocalCheckCommandsAndOutput(t *testing.T) {
	t.Parallel()

	reviewer := `phase_dir=''
previous=''
for arg do
  if [ "$previous" = '--add-dir' ]; then phase_dir=$arg; fi
  previous=$arg
 done
if ! /usr/bin/grep -Fq '## Local checks' "$phase_dir/input.md" ||
   ! /usr/bin/grep -Fq '"command": "check_state=ready; printf tests-passed"' "$phase_dir/input.md" ||
   ! /usr/bin/grep -Fq '"status": "pass"' "$phase_dir/input.md" ||
   ! /usr/bin/grep -Fq '"output": "tests-passed"' "$phase_dir/input.md" ||
   ! /usr/bin/grep -Fq '"output": "lint-passed"' "$phase_dir/input.md"; then
  printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"schema_version":4,"criteria":[],"human_action":null,"prior_findings":[],"status":"failed","summary":"Reviewer missing local checks context","findings":[]}}'
  exit 0
fi
` + testkit.ApprovedReview
	_, rt, api, _, cfg, r := testkit.TmuxFlow(t, testkit.ReviewScript(reviewer))
	cfg.Repositories[0].Checks = []string{"check_state=ready; printf tests-passed", `[ "$check_state" = ready ]; printf lint-passed`}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	var run workflow.Run
	testkit.WaitForWithin(t, 15*time.Second, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != 1 {
			return false
		}
		run = runs[0]
		return run.State == workflow.WaitingForCI || run.State == workflow.NeedsAttention
	})
	if run.State != workflow.WaitingForCI {
		t.Fatalf("reviewer could not consume local check evidence: state=%s error=%s", run.State, run.LastErrorMessage)
	}
}

func TestFixCommentsIdentifyLocalCheckRetryAttempts(t *testing.T) {
	t.Parallel()

	fix := `case "$*" in *fix-1-1*) printf broken > feature.txt ;; *) printf fixed > feature.txt ;; esac
` + testkit.FixedReport
	_, rt, api, _, cfg, r := testkit.TmuxFlow(t, testkit.LoopScript(fix))
	cfg.Repositories[0].Checks = []string{"if /usr/bin/grep -q broken feature.txt; then printf fix-check-failed; exit 4; fi"}
	publisher := &testkit.ReportGitHub{BranchGitHub: testkit.BranchGitHub{FakeGitHub: api}}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: publisher, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	var retryComment string
	for _, body := range publisher.Bodies {
		if strings.Contains(body, "Fix report · round 1, attempt 2") {
			retryComment = body
		}
	}
	if !strings.Contains(retryComment, "Check retry") || !strings.Contains(retryComment, `"check_retry_of":`) {
		t.Fatalf("fix comment lost local check retry context: %s", retryComment)
	}
}

func TestFinishedChecksRecoverPerCommandEvidenceAfterRestart(t *testing.T) {
	t.Parallel()

	release := filepath.Join(t.TempDir(), "release-checks")
	_, rt, api, _, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	commands := []string{"printf first-passed", "printf second-failed; while [ ! -f '" + release + "' ]; do /bin/sleep 0.02; done; exit 7", "printf should-not-run"}
	cfg.Repositories[0].Checks = commands
	cfg.Repositories[0].CheckRetries = 0
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	var run workflow.Run
	testkit.WaitForWithin(t, 15*time.Second, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != 1 {
			return false
		}
		run = runs[0]
		return len(run.SystemSteps) == 1 && run.SystemSteps[0].Status == "running"
	})
	ref, err := s.Watch(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WaitForWithin(t, 5*time.Second, func() bool {
		out, _ := r.ReadFile(context.Background(), filepath.Join(ref.PhaseDir, "events.jsonl"))
		return strings.Contains(string(out), "second-failed")
	})
	root := rt.Workspace.Root
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteFile(context.Background(), release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	testkit.AwaitSetupSession(t, r, ref)
	reopened, err := app.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	restarted, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(reopened), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	recovered := testkit.Finish(t, restarted, workflow.NeedsAttention, workflow.Implement)
	if len(recovered.SystemSteps) != 1 || len(recovered.SystemSteps[0].Results) != 3 {
		t.Fatalf("restart lost command evidence: %+v", recovered.SystemSteps)
	}
	results := recovered.SystemSteps[0].Results
	if results[0].Command != commands[0] || results[0].Status != "pass" || results[0].Output != "first-passed" || results[0].ExitCode == nil || *results[0].ExitCode != 0 {
		t.Fatalf("passed command evidence=%+v", results[0])
	}
	if results[1].Command != commands[1] || results[1].Status != "fail" || results[1].Output != "second-failed" || results[1].ExitCode == nil || *results[1].ExitCode != 7 {
		t.Fatalf("failing command evidence=%+v", results[1])
	}
	if results[2].Command != commands[2] || results[2].Status != "skipped" || results[2].ExitCode != nil || results[2].Output != "" {
		t.Fatalf("skipped command evidence=%+v", results[2])
	}
}

func TestChecksTimeoutIdentifiesInterruptedCommandAndSkipsLaterCommands(t *testing.T) {
	t.Parallel()

	_, rt, api, _, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	commands := []string{"printf before-timeout; /bin/sleep 60", "printf should-not-run"}
	cfg.Repositories[0].Checks = commands
	cfg.Repositories[0].CommandTimeout = 400 * time.Millisecond
	cfg.Repositories[0].CheckRetries = 0
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Implement)
	if len(run.SystemSteps) != 1 || !run.SystemSteps[0].TimedOut || len(run.SystemSteps[0].Results) != 2 {
		t.Fatalf("timeout evidence=%+v", run.SystemSteps)
	}
	results := run.SystemSteps[0].Results
	if results[0].Command != commands[0] || results[0].Status != "fail" || !strings.Contains(results[0].Output, "before-timeout") || !strings.Contains(results[0].Output, "Timed out") || results[0].ExitCode == nil || *results[0].ExitCode == 0 || results[1].Status != "skipped" {
		t.Fatalf("interrupted command was not identified: %+v", results)
	}
}

func TestChangedChecksWaitForLiveListThenRequireNewListBeforePublication(t *testing.T) {
	t.Parallel()

	release := filepath.Join(t.TempDir(), "release-checks")
	_, rt, api, _, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	cfg.Repositories[0].Checks = []string{"printf original-list; while [ ! -f '" + release + "' ]; do /bin/sleep 0.02; done"}
	cfg.Repositories[0].CheckRetries = 0
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	var run workflow.Run
	testkit.WaitForWithin(t, 15*time.Second, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != 1 {
			return false
		}
		run = runs[0]
		return len(run.SystemSteps) == 1 && run.SystemSteps[0].Status == "running"
	})
	cfg.Repositories[0].Checks = []string{"printf changed-list-failed; exit 5"}
	restarted, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, err := restarted.Runs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(before[0].SystemSteps) != 1 {
		t.Fatal("configuration change duplicated a live check session")
	}
	if err := r.WriteFile(context.Background(), release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	failed := testkit.Finish(t, restarted, workflow.NeedsAttention, workflow.Implement)
	if api.Creations != 0 || len(failed.SystemSteps) != 2 || failed.SystemSteps[0].Status != "succeeded" || failed.SystemSteps[1].Status != "failed" || failed.SystemSteps[1].Results[0].Output != "changed-list-failed" {
		t.Fatalf("changed command list reused an earlier pass: %+v", failed.SystemSteps)
	}
}
