// Package verification holds agent-reported command evidence, not command execution.
package verification

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const MaxExcerptRunes = 4000
const Schema = `{"type":"array","items":{"type":"object","properties":{"command":{"type":"string"},"outcome":{"type":"string","enum":["passed","failed","skipped"]},"excerpt":{"type":["string","null"]}},"required":["command","outcome","excerpt"],"additionalProperties":false}}`
const Instruction = "Report verification for commands actually run, with passed, failed, or skipped outcomes and a short output excerpt (or null). Describe why a command was skipped. Do not invent runs. Return verification: [] if none were reported. Criteria evidence may reference verification entries by their command."

type Entry struct {
	Command string  `json:"command"`
	Outcome string  `json:"outcome"`
	Excerpt *string `json:"excerpt"`
}

// Parse enforces the strict contract and caps excerpts before reports are saved.
func Parse(data []byte) ([]Entry, error) {
	if len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil, fmt.Errorf("verification must be an array")
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	for _, entry := range raw {
		if len(entry) != 3 || entry["command"] == nil || entry["outcome"] == nil || entry["excerpt"] == nil {
			return nil, fmt.Errorf("verification requires command, outcome, excerpt")
		}
		for _, key := range []string{"command", "outcome"} {
			if bytes.Equal(bytes.TrimSpace(entry[key]), []byte("null")) {
				return nil, fmt.Errorf("verification %s must be a string", key)
			}
		}
	}
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	if err := Validate(entries); err != nil {
		return nil, err
	}
	return Capped(entries), nil
}

func Validate(entries []Entry) error {
	if entries == nil {
		return fmt.Errorf("verification must be an array")
	}
	for _, entry := range entries {
		if strings.TrimSpace(entry.Command) == "" || (entry.Outcome != "passed" && entry.Outcome != "failed" && entry.Outcome != "skipped") {
			return fmt.Errorf("verification requires a nonempty command and passed, failed, or skipped outcome")
		}
	}
	return nil
}

// Capped returns a copy, preserving null excerpts and valid Unicode.
func Capped(entries []Entry) []Entry {
	if entries == nil {
		return nil
	}
	result := make([]Entry, len(entries))
	copy(result, entries)
	for i, entry := range result {
		if entry.Excerpt == nil {
			continue
		}
		runes := []rune(*entry.Excerpt)
		if len(runes) > MaxExcerptRunes {
			const suffix = "\n[truncated]"
			excerpt := string(runes[:MaxExcerptRunes-len([]rune(suffix))]) + suffix
			result[i].Excerpt = &excerpt
		}
	}
	return result
}
