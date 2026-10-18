package review

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/rcpassos/mergeyard/internal/acceptance"
	"github.com/rcpassos/mergeyard/internal/contract"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/verification"
	"math/big"
	"strings"
)

type Response struct {
	FindingID  string `json:"finding_id"`
	Resolution string `json:"resolution"`
	Note       string `json:"note"`
}

type FixReport struct {
	Verification  []verification.Entry  `json:"verification"`
	Criteria      []acceptance.Evidence `json:"criteria"`
	HumanAction   *fault.HumanAction    `json:"human_action"`
	SchemaVersion int                   `json:"schema_version"`
	Status        string                `json:"status"`
	Summary       string                `json:"summary"`
	Responses     []Response            `json:"responses"`
}

func ParseFix(data []byte) (FixReport, error) { return ParseFixVersion(data, contract.CurrentVersion) }

// ParseFixVersion preserves contracts of attempts launched before upgrade.
func ParseFixVersion(data []byte, expectedVersion int64) (FixReport, error) {
	requirement, ok := contract.For(contract.Fix, int(expectedVersion))
	if !ok {
		return FixReport{}, fmt.Errorf("unsupported fix schema version")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return FixReport{}, err
	}
	if !requirement.Matches(fields) {
		return FixReport{}, fmt.Errorf("fix requires %s", requirement)
	}
	// Validate the required scalar fields before inspecting responses.
	for _, key := range []string{"schema_version", "status", "summary", "responses"} {
		if fields[key] == nil || bytes.Equal(bytes.TrimSpace(fields[key]), []byte("null")) {
			return FixReport{}, fmt.Errorf("missing or null %s", key)
		}
	}
	version, ok := new(big.Rat).SetString(string(fields["schema_version"]))
	if !ok || version.Cmp(big.NewRat(expectedVersion, 1)) != 0 {
		return FixReport{}, fmt.Errorf("unsupported fix schema_version")
	}
	if requirement.Requires("human_action") {
		if _, err := fault.ParseHumanAction(fields["human_action"], stringValue(fields["status"])); err != nil {
			return FixReport{}, err
		}
	}
	if requirement.Requires("criteria") {
		if _, err := acceptance.ParseEvidence(fields["criteria"]); err != nil {
			return FixReport{}, err
		}
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
	fields["schema_version"] = json.RawMessage(fmt.Sprint(expectedVersion))
	normalized, _ := json.Marshal(fields)
	if err := json.Unmarshal(normalized, &report); err != nil {
		return report, err
	}
	if requirement.Requires("verification") {
		var err error
		report.Verification, err = verification.Parse(fields["verification"])
		if err != nil {
			return report, err
		}
	}
	return report, report.Validate()
}

func (r FixReport) Validate() error {
	requirement, ok := contract.For(contract.Fix, r.SchemaVersion)
	if !ok || (r.Status != "success" && r.Status != "blocked" && r.Status != "failed") || strings.TrimSpace(r.Summary) == "" || r.Responses == nil {
		return fmt.Errorf("invalid fix version, status, summary, or responses")
	}
	if requirement.Requires("human_action") {
		if err := r.HumanAction.Validate(r.Status); err != nil {
			return err
		}
	}
	if requirement.Requires("criteria") {
		if err := acceptance.ValidateEvidence(r.Criteria); err != nil {
			return err
		}
	}
	if requirement.Requires("verification") {
		if err := verification.Validate(r.Verification); err != nil {
			return err
		}
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

// FixSchema is the strict fix output schema for the current version.
var FixSchema = contract.Bind(fixSchema)

const fixSchema = `{"type":"object","properties":{"schema_version":{"type":"integer","const":` + contract.VersionToken + `},"status":{"type":"string","enum":["success","blocked","failed"]},"summary":{"type":"string"},"human_action":` + fault.HumanActionSchema + `,"responses":{"type":"array","items":{"type":"object","properties":{"finding_id":{"type":"string"},"resolution":{"type":"string","enum":["fixed","disputed"]},"note":{"type":"string"}},"required":["finding_id","resolution","note"],"additionalProperties":false}},"criteria":` + acceptance.EvidenceSchema + `,"verification":` + verification.Schema + `},"required":["schema_version","status","summary","responses","human_action","criteria","verification"],"additionalProperties":false}`
