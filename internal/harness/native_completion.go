package harness

import (
	"bytes"
	"encoding/json"
	"io"
)

// NativeSucceeded distinguishes normal model completion from semantic report
// validation. A bad task report after native success must never classify usage.
func (*Claude) NativeSucceeded(artifacts PhaseArtifacts) bool {
	if artifacts.ExitCode != 0 {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(artifacts.Stdout))
	type resultEvent struct {
		Type    string `json:"type"`
		IsError *bool  `json:"is_error"`
		Subtype string `json:"subtype"`
	}
	var result *resultEvent
	for {
		var event resultEvent
		if err := decoder.Decode(&event); err == io.EOF {
			break
		} else if err != nil {
			return false
		}
		if event.Type == "result" {
			result = &event
		}
	}
	return result != nil && result.IsError != nil && !*result.IsError && (result.Subtype == "" || result.Subtype == "success")
}

func (*Codex) NativeSucceeded(artifacts PhaseArtifacts) bool {
	if artifacts.ExitCode != 0 {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(artifacts.Stdout))
	completed := false
	for {
		var event codexEvent
		if err := decoder.Decode(&event); err == io.EOF {
			break
		} else if err != nil {
			return false
		}
		switch event.Type {
		case "turn.completed":
			completed = true
		case "turn.failed", "error":
			return false
		}
	}
	return completed
}
