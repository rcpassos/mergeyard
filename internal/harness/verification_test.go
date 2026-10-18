package harness_test

import (
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/workflow"
	"strings"
	"testing"
)

func TestVerificationNativeContracts(t *testing.T) {
	for _, adapter := range []harness.HarnessAdapter{harness.NewClaude(config.Claude{}), harness.NewCodex(config.Codex{})} {
		for _, kind := range []workflow.Phase{workflow.Implement, workflow.Fix} {
			for _, tc := range []struct {
				name, evidence string
				valid          bool
			}{
				{"empty", `[]`, true},
				{"passed", `[{"command":"go test ./...","outcome":"passed","excerpt":"ok"}]`, true},
				{"failed", `[{"command":"go test ./...","outcome":"failed","excerpt":null}]`, true},
				{"skipped", `[{"command":"go test ./...","outcome":"skipped","excerpt":"Unavailable"}]`, true},
				{"missing", ``, false}, {"null", `null`, false}, {"object", `{}`, false},
				{"null entry", `[null]`, false},
				{"missing excerpt", `[{"command":"test","outcome":"passed"}]`, false},
				{"wrong outcome", `[{"command":"test","outcome":"success","excerpt":null}]`, false},
				{"empty command", `[{"command":" ","outcome":"passed","excerpt":null}]`, false},
				{"null command", `[{"command":null,"outcome":"passed","excerpt":null}]`, false},
				{"wrong excerpt type", `[{"command":"test","outcome":"passed","excerpt":42}]`, false},
				{"extra field", `[{"command":"test","outcome":"passed","excerpt":null,"extra":true}]`, false},
				{"wrong key case", `[{"Command":"test","outcome":"passed","excerpt":null}]`, false},
			} {
				t.Run(adapter.Type()+string(kind)+tc.name, func(t *testing.T) {
					raw := `{"schema_version":4,"criteria":[],"human_action":null,"status":"success","summary":"Done"`
					if kind == workflow.Fix {
						raw += `,"responses":[]`
					}
					if tc.evidence != "" {
						raw += `,"verification":` + tc.evidence
					}
					raw += `}`
					artifacts := harness.PhaseArtifacts{Stdout: []byte(`{"type":"result","is_error":false,"structured_output":` + raw + `}`)}
					if adapter.Type() == "codex" {
						artifacts.Stdout = []byte(`{"type":"thread.started","thread_id":"01a10c5c-8a43-7c52-ba89-5a6d6ec1e22c"}` + "\n" + `{"type":"turn.completed"}`)
						artifacts.Files = map[string][]byte{"last-message.json": []byte(raw)}
					}
					result, err := adapter.ParseResult(harness.PhaseContext{Phase: kind}, artifacts)
					if (err == nil) != tc.valid {
						t.Fatalf("valid=%v result=%+v err=%v", tc.valid, result, err)
					}
				})
			}
		}
	}
}

func TestVerificationExcerptsAreCappedBeforePersistence(t *testing.T) {
	for _, kind := range []workflow.Phase{workflow.Implement, workflow.Fix} {
		raw := `{"schema_version":4,"criteria":[],"human_action":null,"status":"success","summary":"Done","verification":[{"command":"test","outcome":"passed","excerpt":"` + strings.Repeat("é", 5000) + `"}]`
		if kind == workflow.Fix {
			raw += `,"responses":[]`
		}
		raw += `}`
		result, err := harness.NewClaude(config.Claude{}).ParseResult(harness.PhaseContext{Phase: kind}, harness.PhaseArtifacts{Stdout: []byte(`{"type":"result","is_error":false,"structured_output":` + raw + `}`)})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Verification) != 1 || len([]rune(*result.Verification[0].Excerpt)) != 4000 || !strings.HasSuffix(*result.Verification[0].Excerpt, "[truncated]") {
			t.Fatalf("excerpt not safely capped: %+v", result)
		}
	}
}
