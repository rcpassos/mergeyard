package scheduler_test

import (
	"context"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/harness/registry"
	"github.com/rcpassos/mergeyard/internal/scheduler/testkit"
)

// TestRecordedPermissionsMatchTheCommandThatRan reads every attempt's audit
// record back out of the command the scheduler actually launched, including the
// editing tools a Claude review removes. This is the persisted chain: adapter
// report, launch journal, phase and review tables.
func TestRecordedPermissionsMatchTheCommandThatRan(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		agent     string
		configure func(*config.Config)
	}{
		{"claude permission mode and tools", registry.Claude, func(cfg *config.Config) {
			cfg.Agents.Claude.PermissionMode = "acceptEdits"
			cfg.Agents.Claude.AllowedTools = []string{"Read", "Bash(go test *)"}
		}},
		{"codex sandbox and network", registry.Codex, func(cfg *config.Config) {
			cfg.Agents.Codex.Sandbox, cfg.Agents.Codex.NetworkAccess = "workspace-write", true
		}},
		{"codex full access ignores the network toggle", registry.Codex, func(cfg *config.Config) {
			cfg.Agents.Codex.Sandbox, cfg.Agents.Codex.NetworkAccess = "danger-full-access", false
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder, runtime := testkit.PermissionFlow(t, tc.agent, tc.configure)
			rows, err := runtime.DB.QueryContext(context.Background(), `SELECT a.phase,a.round,a.attempt,COALESCE(a.permissions,''),COALESCE(r.permission_mode,''),COALESCE(r.allowed_tools_json,''),COALESCE(f.permission_mode,''),COALESCE(f.allowed_tools_json,'')
 FROM phase_attempts a
 LEFT JOIN review_attempts r ON r.attempt_id=a.id
 LEFT JOIN fix_attempts f ON f.attempt_id=a.id
 WHERE a.step='' ORDER BY a.phase,a.round,a.attempt`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			attempts := 0
			for rows.Next() {
				var phase, summary, reviewMode, reviewTools, fixMode, fixTools string
				var round, attempt int
				if err := rows.Scan(&phase, &round, &attempt, &summary, &reviewMode, &reviewTools, &fixMode, &fixTools); err != nil {
					t.Fatal(err)
				}
				request, ok := recorder.Ran(phase, round, attempt)
				if !ok {
					t.Fatalf("%s attempt %d/%d has no launched command", phase, round, attempt)
				}
				wantSummary, wantMode, wantTools := testkit.CommandPermissions(t, tc.agent, request.Command.Args)
				if summary != wantSummary {
					t.Fatalf("%s attempt %d/%d recorded permissions %q; the command recorded %q", phase, round, attempt, summary, wantSummary)
				}
				switch phase {
				case "review":
					testkit.AssertAttemptPermissions(t, phase, round, attempt, wantMode, wantTools, reviewMode, reviewTools)
				case "fix":
					testkit.AssertAttemptPermissions(t, phase, round, attempt, wantMode, wantTools, fixMode, fixTools)
				}
				attempts++
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			// One implement, two reviews and one fix ran in this flow.
			if attempts != 4 {
				t.Fatalf("checked %d attempts, want 4", attempts)
			}
		})
	}
}
