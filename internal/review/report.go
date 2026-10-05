// Package review defines the independent review report and durable display data.
package review

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	SchemaVersion int       `json:"schema_version"`
	Status        string    `json:"status"`
	Summary       string    `json:"summary"`
	Findings      []Finding `json:"findings"`
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
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return Report{}, err
	}
	if len(fields) != 4 || fields["schema_version"] == nil || fields["status"] == nil || fields["summary"] == nil || fields["findings"] == nil {
		return Report{}, fmt.Errorf("review requires only schema_version, status, summary, findings")
	}
	version, ok := new(big.Rat).SetString(string(fields["schema_version"]))
	if !ok || version.Cmp(big.NewRat(1, 1)) != 0 {
		return Report{}, fmt.Errorf("schema_version must be 1")
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
	// The exact version was checked above; normalize numeric spellings such as 1.0.
	fields["schema_version"] = json.RawMessage("1")
	normalized, _ := json.Marshal(fields)
	var report Report
	if err := json.Unmarshal(normalized, &report); err != nil {
		return Report{}, err
	}
	return report, report.Validate()
}

func (r Report) Validate() error {
	if r.SchemaVersion != 1 || r.Findings == nil {
		return fmt.Errorf("invalid review version or findings")
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

const Schema = `{"type":"object","properties":{"schema_version":{"type":"integer","const":1},"status":{"type":"string","enum":["approved","changes_required","blocked","failed"]},"summary":{"type":"string"},"findings":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"severity":{"type":"string","enum":["blocking","warning","note"]},"title":{"type":"string"},"details":{"type":"string"},"file":{"type":["string","null"]},"line":{"type":["integer","null"],"minimum":1}},"required":["id","severity","title","details","file","line"],"additionalProperties":false}}},"required":["schema_version","status","summary","findings"],"additionalProperties":false}`
