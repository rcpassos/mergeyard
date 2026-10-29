package integration_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestFinalBaseVerificationCannotMarkReadyAfterDeadline(t *testing.T) {
	t.Parallel()

	_, rt, base, _, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	git := &testkit.DeadlineBaseGit{Manager: testkit.TestGitManager(rt.Workspace), After: func() { now = now.Add(2 * time.Hour) }}
	api := testkit.PassingBaseAPI(base)
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Git: git, Runner: r, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, err := rt.Workflow.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if api.ReadyCalls != 0 || saved.State != workflow.NeedsAttention || saved.LastErrorCode != "ci.wait_timeout" || saved.CI.ReadyStarted {
		t.Fatalf("late readiness: calls=%d state=%s code=%s intent=%v", api.ReadyCalls, saved.State, saved.LastErrorCode, saved.CI.ReadyStarted)
	}
}

func TestBaseFailuresHonorCIActionPriorityAndUncertainty(t *testing.T) {
	t.Parallel()

	for _, failure := range []string{"conflict", "verification"} {
		for _, conclusion := range []string{"cancelled", "action_required", "unavailable", "merge unavailable"} {
			t.Run(failure+"/"+conclusion, func(t *testing.T) {
				t.Parallel()

				_, rt, base, remote, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
				api := testkit.PassingBaseAPI(base)
				api.Evidence = ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: conclusion}}}
				if conclusion == "unavailable" {
					api.QueryError = &fault.Error{Code: "github.unavailable"}
				}
				if conclusion == "merge unavailable" {
					api.QueryError = &fault.Error{Code: "github.merge_evidence_unknown"}
					api.Evidence.Checks[0].Conclusion = "cancelled"
				}
				if failure == "verification" {
					cfg.Repositories[0].VerifyCommands = []string{"exit 7"}
				}
				s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
				if err != nil {
					t.Fatal(err)
				}
				run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
				if failure == "conflict" {
					testkit.AdvanceBase(t, remote, "feature.txt", "conflicting base\n")
				}
				if err := s.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				saved, err := rt.Workflow.Get(context.Background(), run.ID)
				if err != nil {
					t.Fatal(err)
				}
				expected := workflow.NeedsAttention
				code := "ci.action_required"
				if conclusion == "unavailable" {
					expected = workflow.WaitingForCI
					code = ""
				}
				if saved.State != expected || saved.LastErrorCode != code || saved.ApprovedSHA != run.ApprovedSHA || len(saved.FixHistory) != 0 || len(saved.SystemSteps) != 0 {
					t.Fatalf("base bypassed CI priority: state=%s code=%s approval=%s steps=%d", saved.State, saved.LastErrorCode, saved.ApprovedSHA, len(saved.SystemSteps))
				}
			})
		}
	}
}

func TestCodexBaseConflictRepairOnlyEditsFilesAndPreservesHeadTree(t *testing.T) {
	t.Parallel()

	_, rt, base, remote, cfg, r := testkit.TmuxPairingFlow(t, "codex", "claude", testkit.DisputedReport)
	script := strings.Replace(testkit.CiFixScript(`printf '%s\n' implemented > README.md; `+testkit.CiFixedReport), "'implemented' > feature.txt", "'implemented' > README.md", 1)
	for _, exe := range []struct{ Path, body string }{{cfg.Agents.Claude.Executable, script}, {cfg.Agents.Codex.Executable, testkit.NativeCodexScript(script)}} {
		if err := os.WriteFile(exe.Path, []byte("#!/bin/sh\n"+exe.body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	api := testkit.PassingBaseAPI(base)
	deps := scheduler.Dependencies{GitHub: testkit.CiBranchGitHub{CiGitHub: api}, Runner: r}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
	if err != nil {
		t.Fatal(err)
	}
	before := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	baseSHA := testkit.AdvanceBase(t, remote, "README.md", "conflicting base\n")
	var ready workflow.Run
	testkit.WaitForWithin(t, 20*time.Second, func() bool {
		if err := s.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		ready, err = rt.Workflow.Get(context.Background(), before.ID)
		if err != nil {
			t.Fatal(err)
		}
		if ready.State == workflow.NeedsAttention {
			t.Fatalf("file-only conflict repair failed: %s %s", ready.LastErrorCode, ready.LastErrorMessage)
		}
		return ready.State == workflow.ReadyToMerge
	})
	if ready.ReviewRound != 2 || ready.Fix.Agent != "codex" {
		t.Fatalf("not independently reviewed: %+v", ready.Fix)
	}
	if testkit.GitCommand(t, remote, "rev-parse", ready.ApprovedSHA+"^{tree}") != testkit.GitCommand(t, remote, "rev-parse", before.ApprovedSHA+"^{tree}") {
		t.Fatal("file-only resolution changed approved contents")
	}
	testkit.GitCommand(t, remote, "merge-base", "--is-ancestor", baseSHA, ready.ApprovedSHA)
}

func TestBaseConflictWithoutSyntheticMergeStillEntersManagedRepair(t *testing.T) {

	_, rt, base, remote, cfg, r := testkit.TmuxFlow(t, testkit.SuccessfulScript)
	api := testkit.PassingBaseAPI(base)
	api.QueryError = &fault.Error{Code: "github.merge_evidence_unknown", Message: "conflict prevents test merge"}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	testkit.AdvanceBase(t, remote, "feature.txt", "conflicting base\n")
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, err := rt.Workflow.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.State != workflow.Active || saved.Phase != workflow.Fix || saved.CI.RepairCause != "git.base_conflict" {
		t.Fatalf("conflict blocked on unavailable synthetic merge: %+v", saved.CI)
	}
}

func TestManagedBaseMergeIntentRecoversBeforeAgentLaunch(t *testing.T) {

	_, rt, base, remote, cfg, r := testkit.TmuxFlow(t, testkit.CiFixScript(`printf '%s\n' implemented > feature.txt; `+testkit.CiFixedReport))
	api := testkit.PassingBaseAPI(base)
	deps := scheduler.Dependencies{GitHub: testkit.CiBranchGitHub{CiGitHub: api}, Runner: r}
	s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
	if err != nil {
		t.Fatal(err)
	}
	run := testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
	baseSHA := testkit.AdvanceBase(t, remote, "feature.txt", "conflicting base\n")
	testkit.Finish(t, s, workflow.Active, workflow.Fix)
	if _, err := rt.DB.Exec(`CREATE TRIGGER reject_merge_prepared BEFORE UPDATE ON ci_waits WHEN NEW.snapshot_json LIKE '%"merge_prepared":true%' BEGIN SELECT RAISE(FAIL,'merge crash'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.Tick(context.Background()); err == nil {
		t.Fatal("expected lost merge acknowledgement")
	}
	saved, err := rt.Workflow.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.CI.Base.MergeStarted || saved.CI.Base.MergePrepared || len(saved.FixHistory) != 0 {
		t.Fatal("merge intent not retained before agent launch")
	}
	tree := testkit.SetupWorktree(t, rt, run.ID)
	if got := testkit.GitCommand(t, tree, "rev-parse", "MERGE_HEAD"); got != baseSHA {
		t.Fatal("pinned merge was not started")
	}
	if err := os.WriteFile(tree+"/feature.txt", []byte("implemented\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.DB.Exec("DROP TRIGGER reject_merge_prepared"); err != nil {
		t.Fatal(err)
	}
	s, err = testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
	if err != nil {
		t.Fatal(err)
	}
	ready := testkit.Finish(t, s, workflow.ReadyToMerge, workflow.Review)
	if len(ready.FixHistory) != 1 || !ready.Fix.CI.Base.MergePrepared || ready.ReviewRound != 2 {
		t.Fatal("merge recovery duplicated or skipped the fix")
	}
	testkit.GitCommand(t, remote, "merge-base", "--is-ancestor", baseSHA, ready.ApprovedSHA)
}

func TestRetryManagedConflictPreservesMergeAndRechecksCI(t *testing.T) {

	for _, mode := range []string{"passed", "merge unavailable", "cancelled", "unavailable"} {
		t.Run(mode, func(t *testing.T) {

			blockedReport := `printf '%s\n' '{"type":"result","is_error":false,"structured_output":{"schema_version":3,"criteria":[],"human_action":{"decision":"Confirm resolution","files":[],"options":[]},"status":"blocked","summary":"Need guidance","responses":[]}}'`
			fix := `printf '%s\n' implemented > feature.txt; case "$*" in *fix-1-1*) ` + blockedReport + `;; *) ` + testkit.CiFixedReport + `;; esac`
			_, rt, base, remote, cfg, r := testkit.TmuxFlow(t, testkit.CiFixScript(fix))
			api := testkit.PassingBaseAPI(base)
			deps := scheduler.Dependencies{GitHub: testkit.CiBranchGitHub{CiGitHub: api}, Runner: r}
			s, err := testkit.TestScheduler(cfg, testkit.SchedulerResources(rt), deps)
			if err != nil {
				t.Fatal(err)
			}
			testkit.Finish(t, s, workflow.WaitingForCI, workflow.Review)
			baseSHA := testkit.AdvanceBase(t, remote, "feature.txt", "conflicting base\n")
			blocked := testkit.Finish(t, s, workflow.NeedsAttention, workflow.Fix)
			switch mode {
			case "merge unavailable":
				api.QueryError = &fault.Error{Code: "github.merge_evidence_unknown"}
			case "cancelled":
				api.Evidence.Checks[0].Conclusion = "cancelled"
			case "unavailable":
				api.QueryError = &fault.Error{Code: "github.unavailable"}
			}
			_, err = s.Retry(context.Background(), blocked.ID)
			if mode == "cancelled" || mode == "unavailable" {
				if err == nil {
					t.Fatal("unsafe retry authorized")
				}
				tree := testkit.SetupWorktree(t, rt, blocked.ID)
				if got := testkit.GitCommand(t, tree, "rev-parse", "MERGE_HEAD"); got != baseSHA {
					t.Fatal("rejected retry altered merge")
				}
				return
			}
			if err != nil {
				t.Fatalf("managed merge retry rejected: %v", err)
			}
			api.QueryError = nil
			ready := testkit.Finish(t, s, workflow.ReadyToMerge, workflow.Review)
			if ready.Fix.Attempt != 2 || ready.ReviewRound != 2 || !ready.Fix.CI.Base.MergePrepared {
				t.Fatal("retry did not resume same managed repair")
			}
			testkit.GitCommand(t, remote, "merge-base", "--is-ancestor", baseSHA, ready.ApprovedSHA)
		})
	}
}
