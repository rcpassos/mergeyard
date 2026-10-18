package harness_test

import (
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/harness"
)

func TestInteractiveResumeSharesIdentityValidation(t *testing.T) {
	adapters := harness.Adapters(config.Config{Agents: config.Agents{Claude: config.Claude{Executable: "claude"}, Codex: config.Codex{Executable: "codex"}}})
	worktree := t.TempDir()
	command, err := harness.InteractiveResume(adapters, "claude", sessionID, worktree)
	if err != nil || command.Executable != "claude" || command.Dir != worktree || !strings.Contains(strings.Join(command.Args, " "), sessionID) {
		t.Fatalf("resume: %+v %v", command, err)
	}
	for _, tc := range []struct{ name, agent, session, want string }{
		{"missing harness", "", sessionID, "takeover.harness_unknown"},
		{"unknown harness", "other", sessionID, "takeover.harness_unknown"},
		{"missing session", "claude", "", "takeover.session_missing"},
		{"invalid session", "claude", "not-a-uuid", "takeover.session_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := harness.InteractiveResume(adapters, tc.agent, tc.session, worktree)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %s: %v", tc.want, err)
			}
		})
	}
}
