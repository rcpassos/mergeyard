package harness

import (
	"encoding/json"
	"github.com/rcpassos/mergeyard/internal/review"
	"github.com/rcpassos/mergeyard/internal/workflow"
	"math/big"
)

// Validate the small implement contract directly, including required fields,
// exact property names, types, enum values, and additionalProperties: false.
func parseImplementResult(data []byte) (PhaseResult, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return PhaseResult{}, phaseError("phase.result_invalid", "Structured output must be an implement result object", err)
	}
	if len(fields) != 3 || fields["schema_version"] == nil || fields["status"] == nil || fields["summary"] == nil {
		return PhaseResult{}, phaseError("phase.result_invalid", "Implement result requires only schema_version, status, and summary", nil)
	}
	// Compare exactly: float64 could round a different schema version to 1.
	version, ok := new(big.Rat).SetString(string(fields["schema_version"]))
	if !ok || version.Cmp(big.NewRat(1, 1)) != 0 {
		return PhaseResult{}, phaseError("phase.result_invalid", "Implement result schema_version must be 1", nil)
	}
	var status, summary *string
	if err := json.Unmarshal(fields["status"], &status); err != nil || status == nil || (*status != "success" && *status != "blocked" && *status != "failed") {
		return PhaseResult{}, phaseError("phase.result_invalid", "Implement result status must be success, blocked, or failed", err)
	}
	if err := json.Unmarshal(fields["summary"], &summary); err != nil || summary == nil {
		return PhaseResult{}, phaseError("phase.result_invalid", "Implement result summary must be a string", err)
	}
	return PhaseResult{SchemaVersion: 1, Status: *status, Summary: *summary}, nil
}

// The same semantic contracts apply regardless of the native harness envelope.
func parsePhaseResult(phase workflow.Phase, data []byte) (PhaseResult, error) {
	switch phase {
	case workflow.Review:
		report, err := review.Parse(data)
		if err != nil {
			return PhaseResult{}, phaseError("phase.result_invalid", "Invalid structured review report", err)
		}
		return PhaseResult{SchemaVersion: report.SchemaVersion, Status: report.Status, Summary: report.Summary, Findings: report.Findings}, nil
	case workflow.Fix:
		report, err := review.ParseFix(data)
		if err != nil {
			return PhaseResult{}, phaseError("phase.result_invalid", "Invalid structured fix report", err)
		}
		return PhaseResult{SchemaVersion: report.SchemaVersion, Status: report.Status, Summary: report.Summary, Responses: report.Responses}, nil
	default:
		return parseImplementResult(data)
	}
}
