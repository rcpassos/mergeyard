package scheduler_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/ci"
	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

type deadlineBaseGit struct {
	*managedgit.Manager
	calls int
	after func()
}

func (g *deadlineBaseGit) CheckBase(ctx context.Context, run managedgit.Run, branch, head string) (managedgit.BaseMerge, error) {
	v, err := g.Manager.CheckBase(ctx, run, branch, head)
	g.calls++
	if g.calls == 2 {
		g.after()
	}
	return v, err
}

func TestFinalBaseVerificationCannotMarkReadyAfterDeadline(t *testing.T) {
	_, rt, base, _, cfg, r := localFlow(t, successfulScript)
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	git := &deadlineBaseGit{Manager: managedgit.New(rt.Workspace), after: func() { now = now.Add(2 * time.Hour) }}
	api := passingBaseAPI(base)
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Git: git, Runner: r, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, err := rt.Workflow.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if api.readyCalls != 0 || saved.State != workflow.NeedsAttention || saved.LastErrorCode != "ci.wait_timeout" || saved.CI.ReadyStarted {
		t.Fatalf("late readiness: calls=%d state=%s code=%s intent=%v", api.readyCalls, saved.State, saved.LastErrorCode, saved.CI.ReadyStarted)
	}
}

func TestBaseFailuresHonorCIActionPriorityAndUncertainty(t *testing.T) {
	for _, failure := range []string{"conflict", "verification"} {
		for _, conclusion := range []string{"cancelled", "action_required", "unavailable", "merge unavailable"} {
			t.Run(failure+"/"+conclusion, func(t *testing.T) {
				_, rt, base, remote, cfg, r := localFlow(t, successfulScript)
				api := passingBaseAPI(base)
				api.evidence = ci.Evidence{Checks: []ci.Check{{Name: "build", Source: "check", Status: "completed", Conclusion: conclusion}}}
				if conclusion == "unavailable" {
					api.queryError = &fault.Error{Code: "github.unavailable"}
				}
				if conclusion == "merge unavailable" {
					api.queryError = &fault.Error{Code: "github.merge_evidence_unknown"}
					api.evidence.Checks[0].Conclusion = "cancelled"
				}
				if failure == "verification" {
					cfg.Repositories[0].VerifyCommands = []string{"exit 7"}
				}
				s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
				if err != nil {
					t.Fatal(err)
				}
				run := finish(t, s, workflow.WaitingForCI, workflow.Review)
				if failure == "conflict" {
					advanceBase(t, remote, "feature.txt", "conflicting base\n")
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
	_, rt, base, remote, cfg, r := pairingFlow(t, "codex", "claude", disputedReport)
	script := strings.Replace(ciFixScript(`printf '%s\n' implemented > README.md; `+ciFixedReport), "'implemented' > feature.txt", "'implemented' > README.md", 1)
	for _, exe := range []struct{ path, body string }{{cfg.Agents.Claude.Executable, script}, {cfg.Agents.Codex.Executable, nativeCodexScript(script)}} {
		if err := os.WriteFile(exe.path, []byte("#!/bin/sh\n"+exe.body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	api := passingBaseAPI(base)
	deps := scheduler.Dependencies{GitHub: ciBranchGitHub{api}, Runner: r}
	s, err := scheduler.New(cfg, schedulerResources(rt), deps)
	if err != nil {
		t.Fatal(err)
	}
	before := finish(t, s, workflow.WaitingForCI, workflow.Review)
	baseSHA := advanceBase(t, remote, "README.md", "conflicting base\n")
	var ready workflow.Run
	waitForWithin(t, 20*time.Second, func() bool {
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
	if gitCommand(t, remote, "rev-parse", ready.ApprovedSHA+"^{tree}") != gitCommand(t, remote, "rev-parse", before.ApprovedSHA+"^{tree}") {
		t.Fatal("file-only resolution changed approved contents")
	}
	gitCommand(t, remote, "merge-base", "--is-ancestor", baseSHA, ready.ApprovedSHA)
}

func TestBaseConflictWithoutSyntheticMergeStillEntersManagedRepair(t *testing.T) {
	_, rt, base, remote, cfg, r := localFlow(t, successfulScript)
	api := passingBaseAPI(base)
	api.queryError = &fault.Error{Code: "github.merge_evidence_unknown", Message: "conflict prevents test merge"}
	s, err := scheduler.New(cfg, schedulerResources(rt), scheduler.Dependencies{GitHub: api, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
	advanceBase(t, remote, "feature.txt", "conflicting base\n")
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
	_, rt, base, remote, cfg, r := localFlow(t, ciFixScript(`printf '%s\n' implemented > feature.txt; `+ciFixedReport))
	api := passingBaseAPI(base)
	deps := scheduler.Dependencies{GitHub: ciBranchGitHub{api}, Runner: r}
	s, err := scheduler.New(cfg, schedulerResources(rt), deps)
	if err != nil {
		t.Fatal(err)
	}
	run := finish(t, s, workflow.WaitingForCI, workflow.Review)
	baseSHA := advanceBase(t, remote, "feature.txt", "conflicting base\n")
	finish(t, s, workflow.Active, workflow.Fix)
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
	tree := setupWorktree(t, rt, run.ID)
	if got := gitCommand(t, tree, "rev-parse", "MERGE_HEAD"); got != baseSHA {
		t.Fatal("pinned merge was not started")
	}
	if err := os.WriteFile(tree+"/feature.txt", []byte("implemented\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.DB.Exec("DROP TRIGGER reject_merge_prepared"); err != nil {
		t.Fatal(err)
	}
	s, err = scheduler.New(cfg, schedulerResources(rt), deps)
	if err != nil {
		t.Fatal(err)
	}
	ready := finish(t, s, workflow.ReadyToMerge, workflow.Review)
	if len(ready.FixHistory) != 1 || !ready.Fix.CI.Base.MergePrepared || ready.ReviewRound != 2 {
		t.Fatal("merge recovery duplicated or skipped the fix")
	}
	gitCommand(t, remote, "merge-base", "--is-ancestor", baseSHA, ready.ApprovedSHA)
}
