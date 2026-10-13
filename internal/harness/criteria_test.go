package harness_test

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestPhaseInputsExtractOnlyAcceptanceSection(t *testing.T) {
	for _, body := range []string{
		"## Scope\n- Ignore this\n## aCcEpTaNcE CrItErIa\n- [ ] Runs safely\n- [x] Shows status\n## Other\n- Ignore that",
		"## Scope\n- No criteria here",
	} {
		for _, kind := range []workflow.Phase{workflow.Implement, workflow.Fix, workflow.Review} {
			t.Run(string(kind)+body, func(t *testing.T) {
				ctx := phase(t)
				ctx.Phase = kind
				issue := harness.ImplementInput{IssueBody: body}
				writer := runner.NewLocal(runner.Options{})
				var path string
				var err error
				switch kind {
				case workflow.Implement:
					path, err = harness.WriteImplementInput(context.Background(), writer, ctx, issue)
				case workflow.Fix:
					path, err = harness.WriteFixInput(context.Background(), writer, ctx, harness.FixInput{Issue: issue})
				case workflow.Review:
					path, err = harness.WriteReviewInput(context.Background(), writer, ctx, harness.ReviewInput{Issue: issue})
				}
				if err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				want := "no acceptance criteria found"
				if strings.Contains(body, "Runs safely") {
					want = "AC1: Runs safely\nAC2: Shows status"
				}
				if !strings.Contains(string(data), want) {
					t.Fatalf("missing %q in input: %s", want, data)
				}
				if kind == workflow.Review && !strings.Contains(string(data), "Independently spot-check at least one criterion") {
					t.Fatal("review lacks spot-check instruction")
				}
			})
		}
	}
}

func TestCriteriaNativeContracts(t *testing.T) {
	for _, adapter := range []harness.HarnessAdapter{harness.NewClaude(config.Claude{}), harness.NewCodex(config.Codex{})} {
		for _, kind := range []workflow.Phase{workflow.Implement, workflow.Fix, workflow.Review} {
			for _, tc := range []struct {
				name, criteria string
				valid          bool
			}{
				{"empty", `[]`, true},
				{"missing", ``, false}, {"null", `null`, false},
				{"unknown property", `[{"id":"AC1","met":true,"evidence":"test passed","extra":true}]`, false},
			} {
				t.Run(adapter.Type()+string(kind)+tc.name, func(t *testing.T) {
					version, extra, status := "4", `,"verification":[]`, "success"
					if kind == workflow.Fix {
						extra = `,"verification":[],"responses":[]`
					}
					if kind == workflow.Review {
						version, extra, status = "4", `,"findings":[],"prior_findings":[]`, "approved"
					}
					raw := `{"schema_version":` + version + `,"human_action":null,"status":"` + status + `","summary":"Done"` + extra
					if tc.criteria != "" {
						raw += `,"criteria":` + tc.criteria
					}
					raw += `}`
					artifacts := harness.PhaseArtifacts{Stdout: []byte(`{"type":"result","is_error":false,"structured_output":` + raw + `}`)}
					if adapter.Type() == "codex" {
						artifacts.Stdout = []byte(`{"type":"thread.started","thread_id":"01a10c5c-8a43-7c52-ba89-5a6d6ec1e22c"}` + "\n" + `{"type":"turn.completed"}`)
						artifacts.LastMessage = []byte(raw)
					}
					_, err := adapter.ParseResult(harness.PhaseContext{Phase: kind}, artifacts)
					if (err == nil) != tc.valid {
						t.Fatalf("valid=%v err=%v", tc.valid, err)
					}
				})
			}
		}
	}
}

func TestOlderAttemptsKeepSavedContracts(t *testing.T) {
	for _, adapter := range []harness.HarnessAdapter{harness.NewClaude(config.Claude{}), harness.NewCodex(config.Codex{})} {
		for _, tc := range []struct {
			phase   workflow.Phase
			version int
			raw     string
		}{
			{workflow.Implement, 3, `{"schema_version":3,"criteria":[],"human_action":null,"status":"success","summary":"Done"}`},
			{workflow.Fix, 3, `{"schema_version":3,"criteria":[],"human_action":null,"status":"success","summary":"Done","responses":[]}`},
			{workflow.Implement, 2, `{"schema_version":2,"human_action":null,"status":"success","summary":"Done"}`},
			{workflow.Fix, 2, `{"schema_version":2,"human_action":null,"status":"success","summary":"Done","responses":[]}`},
			{workflow.Review, 2, `{"schema_version":2,"status":"approved","summary":"Done","findings":[],"prior_findings":[]}`},
			{workflow.Review, 3, `{"schema_version":3,"human_action":null,"status":"approved","summary":"Done","findings":[],"prior_findings":[]}`},
		} {
			t.Run(adapter.Type()+string(tc.phase)+strconv.Itoa(tc.version), func(t *testing.T) {
				artifacts := harness.PhaseArtifacts{Stdout: []byte(`{"type":"result","is_error":false,"structured_output":` + tc.raw + `}`)}
				if adapter.Type() == "codex" {
					artifacts.Stdout = []byte(`{"type":"thread.started","thread_id":"01a10c5c-8a43-7c52-ba89-5a6d6ec1e22c"}` + "\n" + `{"type":"turn.completed"}`)
					artifacts.LastMessage = []byte(tc.raw)
				}
				ctx := harness.PhaseContext{Phase: tc.phase, ResultSchemaVersion: tc.version}
				result, err := adapter.ParseResult(ctx, artifacts)
				if err != nil || result.SchemaVersion != tc.version {
					t.Fatalf("saved contract rejected: %+v %v", result, err)
				}
				ctx.ResultSchemaVersion = 0
				if _, err := adapter.ParseResult(ctx, artifacts); err == nil {
					t.Fatal("fresh attempt accepted legacy contract")
				}
			})
		}
	}
}
