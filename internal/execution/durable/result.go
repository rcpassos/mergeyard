package durable

import (
	"context"
	"encoding/json"
	"github.com/rcpassos/mergeyard/internal/acceptance"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/workflow"
	"path/filepath"
)

type TemporaryLimitError struct {
	Outcome harness.FailureClassification
	Exit    int
}

func (*TemporaryLimitError) Error() string {
	return "harness.temporary_limit: Execution interrupted by a temporary usage limit"
}

// Parse semantic results first. Valid blocked/failed task reports and successful
// native completion never enter the classifier, even when tool text names limits.
func (e Executor) Parse(ctx context.Context, runID, attemptID, agent string, phase harness.PhaseContext, artifacts harness.PhaseArtifacts) (harness.PhaseResult, error) {
	// Every in-flight phase uses its saved schema after upgrade. Fresh native
	// adapter calls default to the current contract.
	if phase.PhaseDir != "" {
		schema, err := e.Runner.ReadFile(ctx, filepath.Join(phase.PhaseDir, "schema.json"))
		if err != nil {
			return harness.PhaseResult{}, err
		}
		var contract struct {
			Properties struct {
				SchemaVersion struct {
					Const int `json:"const"`
				} `json:"schema_version"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(schema, &contract); err != nil {
			return harness.PhaseResult{}, err
		}
		if contract.Properties.SchemaVersion.Const < 1 {
			return harness.PhaseResult{}, &fault.Error{Code: "phase.result_invalid", Message: "Saved schema version is missing"}
		}
		phase.ResultSchemaVersion = contract.Properties.SchemaVersion.Const
	}
	result, err := e.Harnesses[agent].ParseResult(phase, artifacts)
	if err == nil && phase.PhaseDir != "" && ((phase.Phase == workflow.Review && result.SchemaVersion >= 4) || (phase.Phase != workflow.Review && result.SchemaVersion >= 3)) {
		raw, readErr := e.Runner.ReadFile(ctx, filepath.Join(phase.PhaseDir, "criteria.json"))
		if readErr != nil {
			return result, readErr
		}
		var criteria []acceptance.Criterion
		if decodeErr := json.Unmarshal(raw, &criteria); decodeErr != nil {
			return result, decodeErr
		}
		if validationErr := acceptance.ValidateIDs(result.Criteria, criteria); validationErr != nil {
			err = &fault.Error{Code: "phase.result_invalid", Message: "Result does not match acceptance criteria", Err: validationErr}
		}
	}
	completed := e.Harnesses[agent].NativeSucceeded(artifacts)
	if completed {
		if proofErr := e.Workflow.RecordCreditProof(ctx, runID, attemptID, e.Now().UTC()); proofErr != nil {
			return result, proofErr
		}
	}
	if err == nil || completed {
		return result, err
	}
	outcome := e.Harnesses[agent].ClassifyFailure(artifacts, e.Now().UTC())
	if outcome.Kind == harness.TemporaryLimit {
		return result, &TemporaryLimitError{Outcome: outcome, Exit: artifacts.ExitCode}
	}
	// The core consumes classified outcomes; native credit signals belong to #83.
	if outcome.Kind == harness.CreditsExhausted {
		return result, &CreditLimitError{Outcome: outcome, Exit: artifacts.ExitCode}
	}
	return result, err
}

type CreditLimitError struct {
	Outcome harness.FailureClassification
	Exit    int
}

func (*CreditLimitError) Error() string {
	return "harness.credits_exhausted: Harness credits are exhausted; explicit recovery is required"
}
