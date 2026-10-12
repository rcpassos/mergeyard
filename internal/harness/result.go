package harness

import (
	"encoding/json"
	"fmt"
	"github.com/rcpassos/mergeyard/internal/acceptance"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/review"
	"github.com/rcpassos/mergeyard/internal/workflow"
	"math/big"
)

// Validate the small implement contract directly, including required fields,
// exact property names, types, enum values, and additionalProperties: false.
func parseImplementResult(data []byte, expectedVersion int64) (PhaseResult, error) {
	if expectedVersion < 1 || expectedVersion > 3 {
		return PhaseResult{}, phaseError("phase.result_invalid", "Unsupported implement schema version", nil)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return PhaseResult{}, phaseError("phase.result_invalid", "Structured output must be an implement result object", err)
	}
	fieldCount := 4
	if expectedVersion == 3 {
		fieldCount = 5
	}
	if expectedVersion == 1 {
		fieldCount = 3
	}
	if len(fields) != fieldCount || fields["schema_version"] == nil || fields["status"] == nil || fields["summary"] == nil {
		return PhaseResult{}, phaseError("phase.result_invalid", "Implement result requires schema_version, status, summary, human_action (v2+), and criteria (v3)", nil)
	}
	// Compare exactly: float64 could round a different schema version to 1.
	version, ok := new(big.Rat).SetString(string(fields["schema_version"]))
	if !ok || version.Cmp(big.NewRat(expectedVersion, 1)) != 0 {
		return PhaseResult{}, phaseError("phase.result_invalid", fmt.Sprintf("Implement result schema_version must be %d", expectedVersion), nil)
	}
	var status, summary *string
	if err := json.Unmarshal(fields["status"], &status); err != nil || status == nil || (*status != "success" && *status != "blocked" && *status != "failed") {
		return PhaseResult{}, phaseError("phase.result_invalid", "Implement result status must be success, blocked, or failed", err)
	}
	if err := json.Unmarshal(fields["summary"], &summary); err != nil || summary == nil {
		return PhaseResult{}, phaseError("phase.result_invalid", "Implement result summary must be a string", err)
	}
	var action *fault.HumanAction
	if expectedVersion >= 2 {
		var err error
		action, err = fault.ParseHumanAction(fields["human_action"], *status)
		if err != nil {
			return PhaseResult{}, phaseError("phase.result_invalid", "Invalid human action", err)
		}
	}
	if expectedVersion == 3 {
		if _, err := acceptance.ParseEvidence(fields["criteria"]); err != nil {
			return PhaseResult{}, phaseError("phase.result_invalid", "Invalid acceptance criteria evidence", err)
		}
	}
	return PhaseResult{Criteria: fields["criteria"], SchemaVersion: int(expectedVersion), Status: *status, Summary: *summary, HumanAction: action}, nil
}

// The same semantic contracts apply regardless of the native harness envelope.
func parsePhaseResult(ctx PhaseContext, data []byte) (PhaseResult, error) {
	switch ctx.Phase {
	case workflow.Review:
		version := ctx.ResultSchemaVersion
		if ctx.AllowLegacyReview {
			version = 1
		}
		if version == 0 {
			version = 4
		}
		report, err := review.ParseVersion(data, version)
		if err != nil {
			return PhaseResult{}, phaseError("phase.result_invalid", "Invalid structured review report", err)
		}
		criteria, _ := json.Marshal(report.Criteria)
		return PhaseResult{Criteria: criteria, HumanAction: report.HumanAction, SchemaVersion: report.SchemaVersion, Status: report.Status, Summary: report.Summary, Findings: report.Findings, PriorFindings: report.PriorFindings}, nil
	case workflow.Fix:
		report, err := review.ParseFixVersion(data, int64(resultVersion(ctx, 3)))
		if err != nil {
			return PhaseResult{}, phaseError("phase.result_invalid", "Invalid structured fix report", err)
		}
		criteria, _ := json.Marshal(report.Criteria)
		return PhaseResult{Criteria: criteria, HumanAction: report.HumanAction, SchemaVersion: report.SchemaVersion, Status: report.Status, Summary: report.Summary, Responses: report.Responses}, nil
	default:
		return parseImplementResult(data, int64(resultVersion(ctx, 3)))
	}
}

func resultVersion(ctx PhaseContext, current int) int {
	if ctx.ResultSchemaVersion != 0 {
		return ctx.ResultSchemaVersion
	}
	return current
}
