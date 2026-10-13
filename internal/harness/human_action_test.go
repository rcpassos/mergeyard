package harness_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/review"
	"github.com/rcpassos/mergeyard/internal/workflow"
)

func TestHumanActionNativeContracts(t *testing.T) {
	for _, adapter := range []harness.HarnessAdapter{harness.NewClaude(config.Claude{}), harness.NewCodex(config.Codex{})} {
		for _, phase := range []workflow.Phase{workflow.Implement, workflow.Review, workflow.Fix} {
			for _, tc := range []struct {
				name, action string
				valid        bool
			}{
				{"exact decision", `{"decision":"Reject duplicates or overwrite?","files":["internal/import.go"],"options":["Reject","Overwrite"]}`, true},
				{"empty options", `{"decision":"Run make setup","files":[],"options":[]}`, true},
				{"null", `null`, false},
				{"empty decision", `{"decision":"","files":[],"options":[]}`, false},
				{"blank decision", `{"decision":"  ","files":[],"options":[]}`, false},
				{"missing files", `{"decision":"Clarify","options":[]}`, false},
				{"null files", `{"decision":"Clarify","files":null,"options":[]}`, false},
				{"null item", `{"decision":"Clarify","files":[null],"options":[]}`, false},
				{"null option", `{"decision":"Clarify","files":[],"options":[null]}`, false},
				{"absolute path", `{"decision":"Clarify","files":["/tmp/a.go"],"options":[]}`, false},
				{"escaping path", `{"decision":"Clarify","files":["../a.go"],"options":[]}`, false},
				{"extra field", `{"decision":"Clarify","files":[],"options":[],"extra":true}`, false},
			} {
				t.Run(adapter.Type()+"/"+string(phase)+"/"+tc.name, func(t *testing.T) {
					version, extra := 4, `,"verification":[]`
					if phase == workflow.Review {
						version, extra = 4, `,"findings":[],"prior_findings":[]`
					}
					if phase == workflow.Fix {
						extra = `,"verification":[],"responses":[]`
					}
					raw := []byte(fmt.Sprintf(`{"schema_version":%d,"criteria":[],"status":"blocked","summary":"Need input","human_action":%s%s}`, version, tc.action, extra))
					artifacts := harness.PhaseArtifacts{Stdout: []byte(`{"type":"result","is_error":false,"structured_output":` + string(raw) + `}`)}
					if adapter.Type() == "codex" {
						artifacts.Stdout = []byte(`{"type":"thread.started","thread_id":"01a10c5c-8a43-7c52-ba89-5a6d6ec1e22c"}` + "\n" + `{"type":"turn.completed"}`)
						artifacts.LastMessage = raw
					}
					result, err := adapter.ParseResult(harness.PhaseContext{Phase: phase}, artifacts)
					if (err == nil) != tc.valid {
						t.Fatalf("result=%+v err=%v", result, err)
					}
					if tc.valid {
						var expected fault.HumanAction
						_ = json.Unmarshal([]byte(tc.action), &expected)
						if !reflect.DeepEqual(result.HumanAction, &expected) {
							t.Fatalf("action lost: %+v", result)
						}
					}
				})
			}
		}
	}
}

func TestHumanActionRequiredAndOnlyForBlocked(t *testing.T) {
	for _, phase := range []workflow.Phase{workflow.Implement, workflow.Review, workflow.Fix} {
		version, extra, status := 4, `,"verification":[]`, "success"
		if phase == workflow.Review {
			version, extra, status = 4, `,"findings":[],"prior_findings":[]`, "approved"
		}
		if phase == workflow.Fix {
			extra = `,"verification":[],"responses":[]`
		}
		for _, action := range []string{"", `,"human_action":{"decision":"Clarify","files":[],"options":[]}`, `,"human_action":null`} {
			raw := fmt.Sprintf(`{"schema_version":%d,"criteria":[],"status":%q,"summary":"Done"%s%s}`, version, status, extra, action)
			_, err := harness.NewClaude(config.Claude{}).ParseResult(harness.PhaseContext{Phase: phase}, harness.PhaseArtifacts{Stdout: []byte(`{"type":"result","is_error":false,"structured_output":` + raw + `}`)})
			if (err == nil) != (action == `,"human_action":null`) {
				t.Fatalf("phase %s accepted wrong action: %s: %v", phase, raw, err)
			}
		}
	}
}

func TestAllPhaseSchemasUseStrictNullableHumanAction(t *testing.T) {
	for _, schema := range []string{string(harness.ImplementSchema()), review.Schema, review.FixSchema} {
		var raw map[string]any
		if err := json.Unmarshal([]byte(schema), &raw); err != nil {
			t.Fatal(err)
		}
		var check func(map[string]any)
		check = func(node map[string]any) {
			if properties, ok := node["properties"].(map[string]any); ok {
				if node["additionalProperties"] != false {
					t.Fatal("object permits additional properties")
				}
				required := node["required"].([]any)
				if len(required) != len(properties) {
					t.Fatal("object has optional properties")
				}
				for name, property := range properties {
					found := false
					for _, key := range required {
						found = found || key == name
					}
					if !found {
						t.Fatalf("missing required %s", name)
					}
					check(property.(map[string]any))
				}
			}
			if items, ok := node["items"].(map[string]any); ok {
				check(items)
			}
		}
		check(raw)
		action := raw["properties"].(map[string]any)["human_action"].(map[string]any)
		if !reflect.DeepEqual(action["type"], []any{"object", "null"}) {
			t.Fatal("action is not nullable")
		}
	}
}
