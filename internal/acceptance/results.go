package acceptance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

type Evidence struct {
	ID       string `json:"id"`
	Met      bool   `json:"met"`
	Evidence string `json:"evidence"`
}

type Verdict struct {
	ID      string `json:"id"`
	Verdict string `json:"verdict"`
	Note    string `json:"note"`
}

const EvidenceSchema = `{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"met":{"type":"boolean"},"evidence":{"type":"string"}},"required":["id","met","evidence"],"additionalProperties":false}}`
const VerdictSchema = `{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"verdict":{"type":"string","enum":["met","not_met","unverified"]},"note":{"type":"string"}},"required":["id","verdict","note"],"additionalProperties":false}}`

var identity = regexp.MustCompile(`^AC[1-9][0-9]*$`)

func parseEntries(data []byte, keys ...string) error {
	if len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return fmt.Errorf("criteria must be an array")
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil {
		return err
	}
	for _, entry := range entries {
		if len(entry) != len(keys) {
			return fmt.Errorf("criterion requires %s", strings.Join(keys, ", "))
		}
		for _, key := range keys {
			if entry[key] == nil || bytes.Equal(bytes.TrimSpace(entry[key]), []byte("null")) {
				return fmt.Errorf("missing or null criterion %s", key)
			}
		}
	}
	return nil
}

func ParseEvidence(data []byte) ([]Evidence, error) {
	if err := parseEntries(data, "id", "met", "evidence"); err != nil {
		return nil, err
	}
	var result []Evidence
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, ValidateEvidence(result)
}

func ValidateEvidence(entries []Evidence) error {
	if entries == nil {
		return fmt.Errorf("criteria must be an array")
	}
	ids := map[string]bool{}
	for _, entry := range entries {
		if !identity.MatchString(entry.ID) || ids[entry.ID] || strings.TrimSpace(entry.Evidence) == "" {
			return fmt.Errorf("criteria require unique AC IDs and nonempty evidence")
		}
		ids[entry.ID] = true
	}
	return nil
}

func ParseVerdicts(data []byte) ([]Verdict, error) {
	if err := parseEntries(data, "id", "verdict", "note"); err != nil {
		return nil, err
	}
	var result []Verdict
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, ValidateVerdicts(result)
}

func ValidateVerdicts(entries []Verdict) error {
	if entries == nil {
		return fmt.Errorf("criteria must be an array")
	}
	ids := map[string]bool{}
	for _, entry := range entries {
		if !identity.MatchString(entry.ID) || ids[entry.ID] || strings.TrimSpace(entry.Note) == "" || (entry.Verdict != "met" && entry.Verdict != "not_met" && entry.Verdict != "unverified") {
			return fmt.Errorf("criteria require unique AC IDs, a valid verdict, and a note")
		}
		ids[entry.ID] = true
	}
	return nil
}

// ValidateIDs binds a report to the issue snapshot supplied to that attempt.
func ValidateIDs(data []byte, expected []Criterion) error {
	var entries []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return err
	}
	remaining := map[string]bool{}
	for _, entry := range expected {
		remaining[entry.ID] = true
	}
	for _, entry := range entries {
		if !remaining[entry.ID] {
			return fmt.Errorf("unknown or duplicate criterion: %s", entry.ID)
		}
		delete(remaining, entry.ID)
	}
	if len(remaining) > 0 {
		return fmt.Errorf("result must account for every acceptance criterion")
	}
	return nil
}
