package harness_test

import (
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestNativeFixContractAndResumedInvocation(t *testing.T) {
	c := harness.NewClaude(config.Claude{PermissionMode: "acceptEdits"})
	ctx := phase(t)
	ctx.Phase, ctx.Resume = workflow.Fix, true
	invocation, err := c.BuildInvocation(ctx, config.Role{Model: "fix-model", Effort: "high", Skills: []string{"fix-skill"}})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(invocation.Args, "\n")
	for _, want := range []string{"--resume\n" + ctx.SessionID, "--model\nfix-model", "--effort\nhigh", "--permission-mode\nacceptEdits", "/fix-skill", "finding_id", "disputed"} {
		if !strings.Contains(args, want) {
			t.Fatalf("missing %q: %s", want, args)
		}
	}
	for _, tc := range []struct {
		report string
		valid  bool
	}{
		{`{"schema_version":1,"status":"success","summary":"Fixed","responses":[{"finding_id":"F1","resolution":"fixed","note":"Added test"}]}`, true},
		{`{"schema_version":1,"status":"success","summary":"Disagree","responses":[{"finding_id":"F1","resolution":"disputed","note":"Already correct"}]}`, true},
		{`{"schema_version":1,"status":"blocked","summary":"Need input","responses":[]}`, true},
		{`{"schema_version":1,"status":"success","summary":"Done"}`, false},
		{`{"schema_version":1,"status":"success","summary":"Done","responses":null}`, false},
		{`{"schema_version":1,"status":"success","summary":"Done","responses":[{"finding_id":"F1","resolution":"approved","note":"Fine"}]}`, false},
		{`{"schema_version":1,"status":"success","summary":"Done","responses":[{"finding_id":"F1","resolution":"fixed","note":""}]}`, false},
		{`{"schema_version":1,"status":"success","summary":"Done","responses":[{"finding_id":"F1","resolution":"fixed","note":"Done","extra":true}]}`, false},
	} {
		result, err := c.ParseResult(ctx, harness.PhaseArtifacts{Stdout: []byte(`{"type":"result","is_error":false,"subtype":"success","structured_output":` + tc.report + `}`)})
		if (err == nil) != tc.valid {
			t.Fatalf("report=%s result=%+v error=%v", tc.report, result, err)
		}
	}
}
