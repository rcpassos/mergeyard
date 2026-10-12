// Package review defines the independent review report and durable display data.
package review

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/rcpassos/mergeyard/internal/fault"
	"math/big"
	"path"
	"strings"
)

type Finding struct {
	ID       string  `json:"id"`
	Severity string  `json:"severity"`
	Title    string  `json:"title"`
	Details  string  `json:"details"`
	File     *string `json:"file"`
	Line     *int    `json:"line"`
}

type Report struct {
	HumanAction   *fault.HumanAction `json:"human_action"`
	SchemaVersion int                `json:"schema_version"`
	Status        string             `json:"status"`
	Summary       string             `json:"summary"`
	Findings      []Finding          `json:"findings"`
	PriorFindings []PriorFinding     `json:"prior_findings"`
}

type PriorFinding struct {
	FindingID string `json:"finding_id"`
	Outcome   string `json:"outcome"`
	Note      string `json:"note"`
}

type OutcomeCounts struct {
	Fixed           int
	NotFixed        int
	NoLongerApplies int
}

func (r Report) PriorCounts() OutcomeCounts {
	var counts OutcomeCounts
	for _, f := range r.PriorFindings {
		switch f.Outcome {
		case "fixed":
			counts.Fixed++
		case "not_fixed":
			counts.NotFixed++
		case "no_longer_applies":
			counts.NoLongerApplies++
		}
	}
	return counts
}

// Snapshot contains the latest attempt, including rejected reports and settings.
type Snapshot struct {
	Agent          string   `json:"agent"`
	SessionID      string   `json:"session_id"`
	Model          string   `json:"model"`
	Effort         string   `json:"effort"`
	Skills         []string `json:"skills"`
	PermissionMode string   `json:"permission_mode"`
	AllowedTools   []string `json:"allowed_tools"`
	Round          int      `json:"round"`
	Attempt        int      `json:"attempt"`
	TargetSHA      string   `json:"target_sha"`
	Status         string   `json:"attempt_status"`
	Restored       bool     `json:"restored"`
	Contaminated   bool     `json:"contaminated"`
	Accepted       bool     `json:"accepted"`
	Report         *Report  `json:"report,omitempty"`
	Error          string   `json:"error,omitempty"`
}

// Parse enforces the native strict contract before using any reported verdict.
func Parse(data []byte) (Report, error) {
	return parse(data, 3)
}

// ParseLegacy is only for attempts launched with the persisted v1 schema.
func ParseLegacy(data []byte) (Report, error) {
	return parse(data, 1)
}

// ParseVersion is for an attempt's saved contract, never for a newly launched review.
func ParseVersion(data []byte, version int) (Report, error) {
	if version < 1 || version > 3 {
		return Report{}, fmt.Errorf("unsupported review schema version")
	}
	return parse(data, int64(version))
}

func parse(data []byte, expectedVersion int64) (Report, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return Report{}, err
	}
	fieldCount := 5
	if expectedVersion == 3 {
		fieldCount = 6
	}
	if expectedVersion == 1 {
		fieldCount = 4
	}
	if len(fields) != fieldCount || fields["schema_version"] == nil || fields["status"] == nil || fields["summary"] == nil || fields["findings"] == nil || (expectedVersion >= 2 && fields["prior_findings"] == nil) {
		return Report{}, fmt.Errorf("review requires schema_version, status, summary, findings, prior_findings (v2+), and human_action (v3)")
	}
	version, ok := new(big.Rat).SetString(string(fields["schema_version"]))
	if !ok || version.Cmp(big.NewRat(expectedVersion, 1)) != 0 {
		return Report{}, fmt.Errorf("schema_version must be %d", expectedVersion)
	}
	if expectedVersion >= 2 {
		if bytes.Equal(bytes.TrimSpace(fields["prior_findings"]), []byte("null")) {
			return Report{}, fmt.Errorf("prior_findings cannot be null")
		}
		var raw []map[string]json.RawMessage
		if err := json.Unmarshal(fields["prior_findings"], &raw); err != nil {
			return Report{}, err
		}
		for _, f := range raw {
			if len(f) != 3 || f["finding_id"] == nil || f["outcome"] == nil || f["note"] == nil {
				return Report{}, fmt.Errorf("prior finding requires finding_id, outcome, note")
			}
		}
	}
	for _, key := range []string{"status", "summary", "findings"} {
		if bytes.Equal(bytes.TrimSpace(fields[key]), []byte("null")) {
			return Report{}, fmt.Errorf("%s cannot be null", key)
		}
	}
	var rawFindings []map[string]json.RawMessage
	if err := json.Unmarshal(fields["findings"], &rawFindings); err != nil {
		return Report{}, err
	}
	for _, f := range rawFindings {
		if len(f) != 6 {
			return Report{}, fmt.Errorf("finding requires id, severity, title, details, file, line")
		}
		for _, key := range []string{"id", "severity", "title", "details", "file", "line"} {
			if f[key] == nil {
				return Report{}, fmt.Errorf("missing finding %s", key)
			}
		}
	}
	if expectedVersion == 3 {
		if _, err := fault.ParseHumanAction(fields["human_action"], stringValue(fields["status"])); err != nil {
			return Report{}, err
		}
	}
	// Normalize equivalent numeric spellings after checking the exact value.
	fields["schema_version"] = json.RawMessage(fmt.Sprint(expectedVersion))
	if expectedVersion == 1 {
		fields["prior_findings"] = json.RawMessage("[]")
	}
	normalized, _ := json.Marshal(fields)
	var report Report
	if err := json.Unmarshal(normalized, &report); err != nil {
		return Report{}, err
	}
	return report, report.Validate()
}

func (r Report) Validate() error {
	if (r.SchemaVersion < 1 || r.SchemaVersion > 3) || r.Findings == nil || (r.SchemaVersion >= 2 && r.PriorFindings == nil) {
		return fmt.Errorf("invalid review version or findings")
	}
	if r.SchemaVersion == 3 {
		if err := r.HumanAction.Validate(r.Status); err != nil {
			return err
		}
	}
	priorIDs := map[string]bool{}
	for _, f := range r.PriorFindings {
		if strings.TrimSpace(f.FindingID) == "" || priorIDs[f.FindingID] || strings.TrimSpace(f.Note) == "" || (f.Outcome != "fixed" && f.Outcome != "not_fixed" && f.Outcome != "no_longer_applies") {
			return fmt.Errorf("prior findings require unique identities, a valid outcome, and a note")
		}
		priorIDs[f.FindingID] = true
	}
	if r.Status != "approved" && r.Status != "changes_required" && r.Status != "blocked" && r.Status != "failed" {
		return fmt.Errorf("invalid review status")
	}
	ids := map[string]bool{}
	blocking := false
	for _, f := range r.Findings {
		if strings.TrimSpace(f.ID) == "" || ids[f.ID] || strings.TrimSpace(f.Title) == "" || strings.TrimSpace(f.Details) == "" {
			return fmt.Errorf("findings require unique nonempty identities, titles, and details")
		}
		ids[f.ID] = true
		if f.Severity != "blocking" && f.Severity != "warning" && f.Severity != "note" {
			return fmt.Errorf("invalid finding severity")
		}
		blocking = blocking || f.Severity == "blocking"
		if f.File != nil && (strings.TrimSpace(*f.File) == "" || path.IsAbs(*f.File) || path.Clean(*f.File) != *f.File || *f.File == ".." || strings.HasPrefix(*f.File, "../") || strings.ContainsAny(*f.File, "\x00\\")) {
			return fmt.Errorf("finding file must be a repository-relative path")
		}
		if f.Line != nil && (*f.Line < 1 || f.File == nil) {
			return fmt.Errorf("finding line requires a file and positive line number")
		}
	}
	if (r.Status == "approved" && blocking) || (r.Status == "changes_required" && !blocking) {
		return fmt.Errorf("verdict contradicts blocking findings")
	}
	return nil
}

// ValidatePriorFindings accounts for the previous round's blockers and rulings
// on disputes. Legacy attempts retain their original contract on restart.
func (r Report) ValidatePriorFindings(round int, previous []Finding, fix *FixReport) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r.SchemaVersion == 1 {
		return nil
	}
	remaining := map[string]bool{}
	if round > 1 || len(previous) > 0 {
		for _, f := range previous {
			if f.Severity == "blocking" {
				remaining[f.ID] = true
			}
		}
	}
	blocking := map[string]bool{}
	for _, f := range r.Findings {
		blocking[f.ID] = f.Severity == "blocking"
	}
	disputed := map[string]bool{}
	if fix != nil {
		for _, response := range fix.Responses {
			disputed[response.FindingID] = response.Resolution == "disputed"
		}
	}
	for _, f := range r.PriorFindings {
		if !remaining[f.FindingID] {
			return fmt.Errorf("prior finding is not a previous blocking finding: %s", f.FindingID)
		}
		if disputed[f.FindingID] && f.Outcome == "fixed" {
			return fmt.Errorf("disputed finding requires not_fixed or no_longer_applies: %s", f.FindingID)
		}
		if f.Outcome == "not_fixed" && !blocking[f.FindingID] {
			return fmt.Errorf("not_fixed finding must remain blocking with the same id: %s", f.FindingID)
		}
		if f.Outcome != "not_fixed" && blocking[f.FindingID] {
			return fmt.Errorf("resolved prior finding cannot remain blocking: %s", f.FindingID)
		}
		delete(remaining, f.FindingID)
	}
	if len(remaining) > 0 {
		return fmt.Errorf("review must account for every previous blocking finding")
	}
	return nil
}

const Schema = `{"type":"object","properties":{"schema_version":{"type":"integer","const":3},"status":{"type":"string","enum":["approved","changes_required","blocked","failed"]},"summary":{"type":"string"},"findings":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"severity":{"type":"string","enum":["blocking","warning","note"]},"title":{"type":"string"},"details":{"type":"string"},"file":{"type":["string","null"]},"line":{"type":["integer","null"],"minimum":1}},"required":["id","severity","title","details","file","line"],"additionalProperties":false}},"human_action":` + fault.HumanActionSchema + `,"prior_findings":{"type":"array","items":{"type":"object","properties":{"finding_id":{"type":"string"},"outcome":{"type":"string","enum":["fixed","not_fixed","no_longer_applies"]},"note":{"type":"string"}},"required":["finding_id","outcome","note"],"additionalProperties":false}}},"required":["schema_version","status","summary","findings","prior_findings","human_action"],"additionalProperties":false}`

func stringValue(data json.RawMessage) string {
	var value string
	_ = json.Unmarshal(data, &value)
	return value
}
