package review

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

type Response struct {
	FindingID  string `json:"finding_id"`
	Resolution string `json:"resolution"`
	Note       string `json:"note"`
}

type FixReport struct {
	SchemaVersion int        `json:"schema_version"`
	Status        string     `json:"status"`
	Summary       string     `json:"summary"`
	Responses     []Response `json:"responses"`
}

func ParseFix(data []byte) (FixReport, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return FixReport{}, err
	}
	if len(fields) != 4 || fields["responses"] == nil {
		return FixReport{}, fmt.Errorf("fix requires schema_version, status, summary, responses")
	}
	// Validate the required scalar fields before inspecting responses.
	for _, key := range []string{"schema_version", "status", "summary", "responses"} {
		if fields[key] == nil || bytes.Equal(bytes.TrimSpace(fields[key]), []byte("null")) {
			return FixReport{}, fmt.Errorf("missing or null %s", key)
		}
	}
	version, ok := new(big.Rat).SetString(string(fields["schema_version"]))
	if !ok || version.Cmp(big.NewRat(1, 1)) != 0 {
		return FixReport{}, fmt.Errorf("schema_version must be 1")
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(fields["responses"], &raw); err != nil {
		return FixReport{}, err
	}
	for _, r := range raw {
		if len(r) != 3 || r["finding_id"] == nil || r["resolution"] == nil || r["note"] == nil {
			return FixReport{}, fmt.Errorf("response requires finding_id, resolution, note")
		}
	}
	var report FixReport
	fields["schema_version"] = json.RawMessage("1")
	normalized, _ := json.Marshal(fields)
	if err := json.Unmarshal(normalized, &report); err != nil {
		return report, err
	}
	return report, report.Validate()
}

func (r FixReport) Validate() error {
	if r.SchemaVersion != 1 || (r.Status != "success" && r.Status != "blocked" && r.Status != "failed") || strings.TrimSpace(r.Summary) == "" || r.Responses == nil {
		return fmt.Errorf("invalid fix version, status, summary, or responses")
	}
	ids := map[string]bool{}
	for _, v := range r.Responses {
		if strings.TrimSpace(v.FindingID) == "" || ids[v.FindingID] || (v.Resolution != "fixed" && v.Resolution != "disputed") || strings.TrimSpace(v.Note) == "" {
			return fmt.Errorf("responses require unique identities, fixed/disputed resolution, and a note")
		}
		ids[v.FindingID] = true
	}
	return nil
}

// ValidateFindings ties responses to the supplied blockers. A successful fix
// accounts for every blocker; blocked/failed attempts may report partial work.
func (r FixReport) ValidateFindings(findings []Finding) error {
	if err := r.Validate(); err != nil {
		return err
	}
	remaining := map[string]bool{}
	for _, f := range findings {
		if f.Severity == "blocking" {
			remaining[f.ID] = true
		}
	}
	for _, v := range r.Responses {
		if !remaining[v.FindingID] {
			return fmt.Errorf("response does not correspond to a supplied blocking finding: %s", v.FindingID)
		}
		delete(remaining, v.FindingID)
	}
	if r.Status == "success" && len(remaining) > 0 {
		return fmt.Errorf("successful fix must respond to every blocking finding")
	}
	return nil
}

const FixSchema = `{"type":"object","properties":{"schema_version":{"type":"integer","const":1},"status":{"type":"string","enum":["success","blocked","failed"]},"summary":{"type":"string"},"responses":{"type":"array","items":{"type":"object","properties":{"finding_id":{"type":"string"},"resolution":{"type":"string","enum":["fixed","disputed"]},"note":{"type":"string"}},"required":["finding_id","resolution","note"],"additionalProperties":false}}},"required":["schema_version","status","summary","responses"],"additionalProperties":false}`
