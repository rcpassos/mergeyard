package testkit

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func MissingConversation(req runner.SessionRequest) []byte {
	id := ""
	for i, arg := range req.Command.Args {
		if (arg == "--resume" || arg == "--session-id") && i+1 < len(req.Command.Args) {
			id = req.Command.Args[i+1]
		}
		if arg == "resume" {
			for j := i + 1; j+1 < len(req.Command.Args); j++ {
				if req.Command.Args[j] == "--" {
					id = req.Command.Args[j+1]
					break
				}
			}
		}
	}
	if id == "" {
		panic("missing-conversation fixture requires a resumed invocation")
	}
	if strings.Contains(filepath.Base(req.Command.Executable), "codex") {
		return []byte("Error: thread/resume: thread/resume failed: no rollout found for thread id " + id + " (code -32600)\n")
	}
	return []byte("No conversation found with session ID: " + id + "\n")
}

func NewRecoveryScenario(t *testing.T, agent string, phase workflow.Phase, outcome string) *Scenario {
	t.Helper()
	flow := NewPairingScenario(t, agent, agent, FixedReport)
	failure := ScenarioOutcome{Exit: 1, StderrFor: MissingConversation}
	recovered := NativeOutcome(FixedReport)
	recovered.Identity = ReplacementID
	recovered.Apply = ScenarioWrite(t, "feature.txt", "fixed")
	switch phase {
	case workflow.Implement:
		recovered = NativeOutcome(SuccessfulScript)
		recovered.Identity = ReplacementID
		run, err := flow.Runtime.Workflow.Transition(t.Context(), "saved-implement", workflow.Request{Trigger: workflow.IssueClaimed, Repository: "owner/repo", IssueNumber: 7})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := flow.Runtime.DB.Exec("UPDATE runs SET implementer_session_id=? WHERE id=?", CodexID, run.ID); err != nil {
			t.Fatal(err)
		}
	case workflow.Review:
		recovered = NativeOutcome(ApprovedAfterFixReview)
		recovered.Identity = ReplacementID
	}
	if outcome == "unrelated" {
		failure.StderrFor = nil
		failure.Stderr = []byte("Authentication failed\n")
	}
	if outcome == "invalid" {
		recovered = NativeOutcome(`{"type":"result","is_error":false,"structured_output":{"status":"invalid"}}`)
		recovered.Identity = ReplacementID
	}
	if outcome == "failed" || outcome == "exhausted" {
		recovered = ScenarioOutcome{Exit: 1, Identity: ReplacementID, Stderr: []byte("temporary execution failure\n")}
	}
	outcomes := []ScenarioOutcome{failure, recovered}
	if phase == workflow.Review {
		outcomes = append([]ScenarioOutcome{NativeOutcome(ChangesReview)}, outcomes...)
	}
	if outcome == "exhausted" {
		failure.Identity = ReplacementID
		outcomes = append(outcomes, failure)
		flow.Cfg.Repositories[0].Implementer.MaxAttempts = 3
		flow.Cfg.Repositories[0].Reviewer.MaxAttempts = 3
	}
	flow.Runner.Outcomes[string(phase)] = outcomes
	return flow
}
