package harness_test

import (
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/harness"
)

func TestResumeRequiresVerifiedRequestedSession(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		adapter := harness.HarnessAdapter(harness.NewClaude(config.Claude{}))
		diagnostic := "No conversation found with session ID: " + sessionID
		if agent == "codex" {
			adapter = harness.NewCodex(config.Codex{})
			diagnostic = "Error: thread/resume: thread/resume failed: no rollout found for thread id " + sessionID + " (code -32600)"
		}
		for _, tc := range []struct {
			name, text string
			recovery   bool
		}{
			{"verified", diagnostic, true},
			{"different session", strings.ReplaceAll(diagnostic, sessionID, "01a10c61-253c-7173-a0d7-d82901ccda96"), false},
			{"authentication", "Authentication failed", false},
			{"configuration", "Unsupported model", false},
			{"unknown", "", false},
			{"quoted diagnostic", "Configuration error: example: " + diagnostic, false},
			{"authentication plus diagnostic", "Authentication failed\n" + diagnostic, false},
		} {
			t.Run(agent+"/"+tc.name, func(t *testing.T) {
				ctx := phase(t)
				ctx.Resume = true
				_, err := adapter.ParseResult(ctx, harness.PhaseArtifacts{Stderr: []byte(tc.text), ExitCode: 1})
				if err == nil || strings.HasPrefix(err.Error(), "harness.session_resume_failed:") != tc.recovery {
					t.Fatalf("diagnostic %q: %v, recovery=%t", tc.text, err, tc.recovery)
				}
			})
		}
	}
}
