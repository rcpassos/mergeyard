package github_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rcpassos/mergeyard/internal/github"
)

func commentHistory(id, author, body string) []byte {
	data, _ := json.Marshal(map[string]any{"data": map[string]any{"node": map[string]any{
		"id": id, "author": map[string]string{"login": author}, "body": body, "lastEditedAt": nil, "editor": nil,
		"userContentEdits": map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}},
	}}})
	return data
}

func TestIssueCommentExcludedEditorCannotSupplyInstructions(t *testing.T) {
	r := &recordedGH{responses: []response{
		{stdout: []byte(`[{"id":1,"node_id":"C1","user":{"login":"alice"},"created_at":"2026-10-08T12:00:00Z","body":"Excluded writer's instructions"}]`)},
		{stdout: []byte(`{"data":{"node":{"id":"C1","author":{"login":"alice"},"body":"Excluded writer's instructions","lastEditedAt":"2026-10-09T12:00:00Z","editor":{"login":"mallory"},"userContentEdits":{"nodes":[{"editedAt":"2026-10-09T12:00:00Z","editor":{"login":"mallory"}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}`)},
	}}
	got, err := github.New(r).ListIssueComments(context.Background(), "o/r", 106, []string{"alice"})
	if err != nil || len(got.Comments) != 0 || got.OmittedUntrusted != 1 {
		t.Fatalf("excluded editor admitted: %+v %v", got, err)
	}
}

func TestIssueCommentHistoryRequiresTrustedEditorsAndStableSnapshot(t *testing.T) {
	base := commentHistory("C1", "alice", "Answer")
	for _, tc := range []struct {
		name, editor string
		broken       string
		wantCode     string
		omitted      int
	}{
		{name: "trusted edit", editor: "bob"},
		{name: "excluded edit", editor: "mallory", omitted: 1},
		{name: "missing editor", editor: "", wantCode: "github.invalid_response"},
		{name: "missing history", broken: "history", wantCode: "github.invalid_response"},
		{name: "changed body", broken: "body", wantCode: "github.invalid_response"},
		{name: "changed author", broken: "author", wantCode: "github.invalid_response"},
		{name: "wrong node", broken: "id", wantCode: "github.invalid_response"},
		{name: "deleted edit", editor: "bob", broken: "deleted", wantCode: "github.invalid_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var payload map[string]any
			json.Unmarshal(base, &payload)
			node := payload["data"].(map[string]any)["node"].(map[string]any)
			if tc.editor != "" || tc.name == "missing editor" {
				node["lastEditedAt"] = "2026-10-09T12:00:00Z"
				actor := any(map[string]string{"login": tc.editor})
				if tc.editor == "" {
					actor = nil
				}
				node["editor"] = actor
				edit := map[string]any{"editedAt": "2026-10-09T12:00:00Z", "editor": actor}
				if tc.broken == "deleted" {
					edit["deletedAt"] = "2026-10-10T12:00:00Z"
				}
				node["userContentEdits"].(map[string]any)["nodes"] = []any{edit}
			}
			switch tc.broken {
			case "history":
				node["userContentEdits"] = nil
			case "body":
				node["body"] = "Raced edit"
			case "author":
				node["author"] = map[string]string{"login": "mallory"}
			case "id":
				node["id"] = "another-comment"
			}
			history, _ := json.Marshal(payload)
			r := &recordedGH{responses: []response{
				{stdout: []byte(`[{"id":1,"node_id":"C1","user":{"login":"alice"},"created_at":"2026-10-08T12:00:00Z","body":"Answer"}]`)},
				{stdout: history},
			}}
			got, err := github.New(r).ListIssueComments(context.Background(), "o/r", 106, []string{"alice", "bob"})
			if tc.wantCode != "" {
				requireCode(t, err, tc.wantCode)
				if len(got.Comments) != 0 {
					t.Fatal("unverified text returned")
				}
				return
			}
			if err != nil || got.OmittedUntrusted != tc.omitted || len(got.Comments) != 1-tc.omitted {
				t.Fatalf("result=%+v err=%v", got, err)
			}
			var input map[string]any
			if err := json.Unmarshal(r.calls[1].input, &input); err != nil {
				t.Fatal(err)
			}
			if input["variables"].(map[string]any)["id"] != "C1" {
				t.Fatal("history lookup not bound to REST comment")
			}
		})
	}
}

func TestIssueCommentChecksHistoricalEditorsOnEveryPage(t *testing.T) {
	for _, editor := range []string{"bob", "mallory"} {
		t.Run(editor, func(t *testing.T) {
			first := `{"data":{"node":{"id":"C1","author":{"login":"alice"},"body":"Answer","lastEditedAt":"2026-10-10T12:00:00Z","editor":{"login":"alice"},"userContentEdits":{"nodes":[{"editedAt":"2026-10-10T12:00:00Z","editor":{"login":"alice"}}],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}}}`
			second := `{"data":{"node":{"id":"C1","author":{"login":"alice"},"body":"Answer","lastEditedAt":"2026-10-10T12:00:00Z","editor":{"login":"alice"},"userContentEdits":{"nodes":[{"editedAt":"2026-10-09T12:00:00Z","editor":{"login":"` + editor + `"}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}`
			r := &recordedGH{responses: []response{
				{stdout: []byte(`[{"id":1,"node_id":"C1","user":{"login":"alice"},"created_at":"2026-10-08T12:00:00Z","body":"Answer"}]`)},
				{stdout: []byte(first)}, {stdout: []byte(second)},
			}}
			got, err := github.New(r).ListIssueComments(context.Background(), "o/r", 106, []string{"alice", "bob"})
			want := 1
			if editor == "mallory" {
				want = 0
			}
			if err != nil || len(got.Comments) != want || got.OmittedUntrusted != 1-want {
				t.Fatalf("result=%+v err=%v", got, err)
			}
			var input map[string]any
			json.Unmarshal(r.calls[2].input, &input)
			if input["variables"].(map[string]any)["cursor"] != "next" {
				t.Fatal("history pagination skipped")
			}
		})
	}
}
