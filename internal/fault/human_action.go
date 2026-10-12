package fault

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
)

// HumanAction is the exact decision or command needed to unblock a run.
type HumanAction struct {
	Decision string   `json:"decision"`
	Files    []string `json:"files"`
	Options  []string `json:"options"`
}

func (a *HumanAction) Validate(status string) error {
	if status != "blocked" {
		if a != nil {
			return fmt.Errorf("human_action must be null unless blocked")
		}
		return nil
	}
	if a == nil || strings.TrimSpace(a.Decision) == "" || a.Files == nil || a.Options == nil {
		return fmt.Errorf("blocked results require a decision, files and options arrays")
	}
	for _, file := range a.Files {
		if strings.TrimSpace(file) == "" || path.IsAbs(file) || path.Clean(file) != file || file == ".." || strings.HasPrefix(file, "../") || strings.ContainsAny(file, "\x00\\") {
			return fmt.Errorf("human_action files must be repository-relative paths")
		}
	}
	return nil
}

// ParseHumanAction checks required fields and types before semantic validation.
func ParseHumanAction(data json.RawMessage, status string) (*HumanAction, error) {
	if data == nil {
		return nil, fmt.Errorf("human_action is required")
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil, (*HumanAction)(nil).Validate(status)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	if len(fields) != 3 {
		return nil, fmt.Errorf("human_action requires only decision, files, options")
	}
	for _, key := range []string{"decision", "files", "options"} {
		if fields[key] == nil || bytes.Equal(bytes.TrimSpace(fields[key]), []byte("null")) {
			return nil, fmt.Errorf("missing or null human_action.%s", key)
		}
	}
	// json.Unmarshal permits null string elements, so inspect each array item.
	for _, key := range []string{"files", "options"} {
		var items []json.RawMessage
		if err := json.Unmarshal(fields[key], &items); err != nil {
			return nil, err
		}
		for _, item := range items {
			var value *string
			if err := json.Unmarshal(item, &value); err != nil || value == nil {
				return nil, fmt.Errorf("human_action.%s must contain strings", key)
			}
		}
	}
	var action HumanAction
	if err := json.Unmarshal(data, &action); err != nil {
		return nil, err
	}
	return &action, action.Validate(status)
}

// Action preserves agent decisions through diagnostic wrappers.
func Action(err error) *HumanAction {
	for err != nil {
		var coded *Error
		if errors.As(err, &coded) && coded.HumanAction != nil {
			return coded.HumanAction
		}
		err = errors.Unwrap(err)
	}
	return nil
}

const HumanActionSchema = `{"type":["object","null"],"properties":{"decision":{"type":"string"},"files":{"type":"array","items":{"type":"string"}},"options":{"type":"array","items":{"type":"string"}}},"required":["decision","files","options"],"additionalProperties":false}`

const HumanActionInstruction = "When status is blocked, return a non-null human_action with the exact question or command the human must answer or run, repository-relative files to inspect, and options (which may be empty). For every other status return human_action: null."
