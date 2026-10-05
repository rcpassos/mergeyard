package harness_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestNativeReviewContract(t *testing.T) {
	for _, tc := range []struct {
		name, report string
		valid        bool
	}{
		{"approved with note", `{"schema_version":1,"status":"approved","summary":"Clean","findings":[{"id":"R1-F1","severity":"note","title":"Optional","details":"Consider later","file":null,"line":null}]}`, true},
		{"blocking", `{"schema_version":1,"status":"changes_required","summary":"Fix","findings":[{"id":"R1-F1","severity":"blocking","title":"Bug","details":"Fails","file":"app.go","line":3}]}`, true},
		{"contradictory approval", `{"schema_version":1,"status":"approved","summary":"Fix","findings":[{"id":"R1-F1","severity":"blocking","title":"Bug","details":"Fails","file":null,"line":null}]}`, false},
		{"changes without blockers", `{"schema_version":1,"status":"changes_required","summary":"Fix","findings":[]}`, false},
		{"missing findings", `{"schema_version":1,"status":"approved","summary":"Clean"}`, false},
		{"null findings", `{"schema_version":1,"status":"approved","summary":"Clean","findings":null}`, false},
		{"missing location", `{"schema_version":1,"status":"approved","summary":"Clean","findings":[{"id":"R1-F1","severity":"note","title":"N","details":"D"}]}`, false},
		{"duplicate ids", `{"schema_version":1,"status":"approved","summary":"Clean","findings":[{"id":"R1-F1","severity":"note","title":"N","details":"D","file":null,"line":null},{"id":"R1-F1","severity":"warning","title":"N","details":"D","file":null,"line":null}]}`, false},
		{"fractional version", `{"schema_version":1.00000000000000001,"status":"approved","summary":"Clean","findings":[]}`, false},
		{"blocked", `{"schema_version":1,"status":"blocked","summary":"Need input","findings":[]}`, true},
		{"failed", `{"schema_version":1,"status":"failed","summary":"Could not run tests","findings":[]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := json.RawMessage(`{"type":"result","is_error":false,"subtype":"success","structured_output":` + tc.report + `}`)
			result, err := harness.NewClaude(config.Claude{}).ParseResult(harness.PhaseContext{Phase: workflow.Review}, harness.PhaseArtifacts{Stdout: output})
			if (err == nil) != tc.valid {
				t.Fatalf("result=%+v err=%v valid=%v", result, err, tc.valid)
			}
		})
	}
}

func TestReviewInvocationKeepsTestsAvailableAndReappliesSettings(t *testing.T) {
	ctx := phase(t)
	ctx.Phase = workflow.Review
	ctx.Resume = true
	role := config.Role{Agent: "claude", Model: "review-model", Effort: "high", Skills: []string{"review-skill"}}
	command, err := harness.NewClaude(config.Claude{PermissionMode: "acceptEdits", AllowedTools: []string{"Read", "Bash(go test *)"}}).BuildInvocation(ctx, role)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(command.Args, "\n")
	for _, text := range []string{"--resume\n" + ctx.SessionID, "--model\nreview-model", "--effort\nhigh", "--permission-mode\nacceptEdits", "--allowedTools\nRead\nBash(go test *)", "--disallowedTools\nEdit\nWrite\nNotebookEdit", "/review-skill", "changes_required"} {
		if !strings.Contains(args, text) {
			t.Fatalf("review invocation missing %q: %s", text, args)
		}
	}
	if strings.Contains(args, "--disallowedTools\nBash") || strings.Contains(args, "--continue") {
		t.Fatal("review cannot run tests or resumes ambiguously")
	}
}
