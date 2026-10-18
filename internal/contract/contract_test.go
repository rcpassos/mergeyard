package contract_test

import (
	"encoding/json"
	"slices"
	"strconv"
	"testing"

	"github.com/rcpassos/mergeyard/internal/contract"
)

// Every phase table is contiguous from version 1 to CurrentVersion and no
// table answers for an unknown version or phase.
func TestTablesCoverEveryVersion(t *testing.T) {
	for _, phase := range []contract.Phase{contract.Implement, contract.Review, contract.Fix} {
		requirement, ok := contract.For(phase, contract.CurrentVersion)
		if !ok || len(requirement.Fields) == 0 {
			t.Fatalf("%s current version missing: %+v ok=%v", phase, requirement, ok)
		}
		if _, ok := contract.For(phase, contract.CurrentVersion+1); ok {
			t.Fatalf("%s answers for a version beyond CurrentVersion", phase)
		}
		for version := 1; version <= contract.CurrentVersion; version++ {
			if requirement, ok := contract.For(phase, version); !ok || len(requirement.Fields) == 0 {
				t.Fatalf("%s v%d missing: %+v ok=%v", phase, version, requirement, ok)
			}
		}
		if _, ok := contract.For(phase, 0); ok {
			t.Fatalf("%s accepted version 0", phase)
		}
	}
	if _, ok := contract.For(contract.Phase("other"), 1); ok {
		t.Fatal("unknown phase accepted")
	}
}

// Every persisted version keeps exactly the fields its parser has always read.
func TestRequirementsByPhaseAndVersion(t *testing.T) {
	want := map[contract.Phase][][]string{
		contract.Implement: {
			{"schema_version", "status", "summary"},
			{"schema_version", "status", "summary", "human_action"},
			{"schema_version", "status", "summary", "human_action", "criteria"},
			{"schema_version", "status", "summary", "human_action", "criteria", "verification"},
		},
		contract.Review: {
			{"schema_version", "status", "summary", "findings"},
			{"schema_version", "status", "summary", "findings", "prior_findings"},
			{"schema_version", "status", "summary", "findings", "prior_findings", "human_action"},
			{"schema_version", "status", "summary", "findings", "prior_findings", "human_action", "criteria"},
		},
		contract.Fix: {
			{"schema_version", "status", "summary", "responses"},
			{"schema_version", "status", "summary", "responses", "human_action"},
			{"schema_version", "status", "summary", "responses", "human_action", "criteria"},
			{"schema_version", "status", "summary", "responses", "human_action", "criteria", "verification"},
		},
	}
	if len(want[contract.Implement]) != contract.CurrentVersion {
		t.Fatalf("fixture covers %d versions, CurrentVersion is %d", len(want[contract.Implement]), contract.CurrentVersion)
	}
	for phase, versions := range want {
		for i, fields := range versions {
			requirement, ok := contract.For(phase, i+1)
			if !ok || !slices.Equal(requirement.Fields, fields) {
				t.Fatalf("%s v%d fields = %v, want %v", phase, i+1, requirement.Fields, fields)
			}
		}
	}
}

func TestRequirementMatchesExactFields(t *testing.T) {
	implement, _ := contract.For(contract.Implement, 2)
	value := json.RawMessage("null")
	for name, tc := range map[string]struct {
		fields map[string]json.RawMessage
		valid  bool
	}{
		"exact":   {map[string]json.RawMessage{"schema_version": value, "status": value, "summary": value, "human_action": value}, true},
		"missing": {map[string]json.RawMessage{"schema_version": value, "status": value, "summary": value}, false},
		"extra":   {map[string]json.RawMessage{"schema_version": value, "status": value, "summary": value, "human_action": value, "criteria": value}, false},
		"null":    {map[string]json.RawMessage{"schema_version": value, "status": value, "summary": value, "human_action": json.RawMessage("null")}, true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := implement.Matches(tc.fields); got != tc.valid {
				t.Fatalf("Matches = %v, want %v", got, tc.valid)
			}
		})
	}
	if !implement.Requires("human_action") || implement.Requires("criteria") {
		t.Fatal("Requires does not read the required fields")
	}
}

// Schemas embed the same literal the parsers enforce.
func TestBindEmbedsCurrentVersion(t *testing.T) {
	want := `{"const":` + strconv.Itoa(contract.CurrentVersion) + `}`
	if got := contract.Bind(`{"const":{{version}}}`); got != want {
		t.Fatalf("Bind = %s, want %s", got, want)
	}
}
