package review_test

import (
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/contract"
	"github.com/rcpassos/mergeyard/internal/review"
)

func TestPriorFindingContract(t *testing.T) {
	base := `{"schema_version":4,"criteria":[],"human_action":null,"status":"approved","summary":"Clean","findings":[],"prior_findings":[]}`
	for _, tc := range []struct {
		name, prior string
		valid       bool
	}{
		{"empty", `[]`, true},
		{"fixed", `[{"finding_id":"F1","outcome":"fixed","note":"Verified"}]`, true},
		{"null", `null`, false},
		{"object", `{}`, false},
		{"unknown outcome", `[{"finding_id":"F1","outcome":"disputed","note":"Verified"}]`, false},
		{"missing note", `[{"finding_id":"F1","outcome":"fixed"}]`, false},
		{"null note", `[{"finding_id":"F1","outcome":"fixed","note":null}]`, false},
		{"empty note", `[{"finding_id":"F1","outcome":"fixed","note":" "}]`, false},
		{"unknown property", `[{"finding_id":"F1","outcome":"fixed","note":"Verified","extra":true}]`, false},
		{"duplicate", `[{"finding_id":"F1","outcome":"fixed","note":"Verified"},{"finding_id":"F1","outcome":"not_fixed","note":"Remains"}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := review.Parse([]byte(strings.Replace(base, `"prior_findings":[]`, `"prior_findings":`+tc.prior, 1)))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
	if _, err := review.Parse([]byte(strings.Replace(base, `,"prior_findings":[]`, "", 1))); err == nil {
		t.Fatal("missing prior_findings accepted")
	}
	legacy := `{"schema_version":1,"status":"approved","summary":"Clean","findings":[]}`
	if _, err := review.Parse([]byte(legacy)); err == nil {
		t.Fatal("new attempt accepted v1")
	}
	report, err := review.ParseLegacy([]byte(legacy))
	if err != nil || report.PriorFindings == nil || len(report.PriorFindings) != 0 {
		t.Fatalf("legacy=%+v err=%v", report, err)
	}
}

// The dashboard's criteria and verification sections follow each version's
// contract rather than hard-coded version comparisons.
func TestReportContractPresenceByVersion(t *testing.T) {
	for version := 1; version <= contract.CurrentVersion; version++ {
		reviewCriteria, fixCriteria, fixVerification := version >= 4, version >= 3, version >= 4
		if got := (review.Report{SchemaVersion: version}).HasCriteria(); got != reviewCriteria {
			t.Fatalf("review v%d HasCriteria = %v, want %v", version, got, reviewCriteria)
		}
		if got := (review.FixReport{SchemaVersion: version}).HasCriteria(); got != fixCriteria {
			t.Fatalf("fix v%d HasCriteria = %v, want %v", version, got, fixCriteria)
		}
		if got := (review.FixReport{SchemaVersion: version}).HasVerification(); got != fixVerification {
			t.Fatalf("fix v%d HasVerification = %v, want %v", version, got, fixVerification)
		}
	}
}

func TestValidatePriorFindings(t *testing.T) {
	blocker := review.Finding{ID: "F1", Severity: "blocking", Title: "Bug", Details: "Broken"}
	warning := review.Finding{ID: "W1", Severity: "warning", Title: "Optional", Details: "Later"}
	disputed := &review.FixReport{Responses: []review.Response{{FindingID: "F1", Resolution: "disputed"}}}
	for _, tc := range []struct {
		name     string
		round    int
		prior    []review.PriorFinding
		findings []review.Finding
		fix      *review.FixReport
		valid    bool
	}{
		{"same round retry accounted", 1, []review.PriorFinding{{"F1", "fixed", "Verified"}}, nil, nil, true},
		{"same round retry omitted", 1, []review.PriorFinding{}, nil, nil, false},
		{"first round", 1, []review.PriorFinding{}, nil, nil, true},
		{"first round has outcome", 1, []review.PriorFinding{{"F1", "fixed", "Verified"}}, nil, nil, false},
		{"omitted blocker", 2, []review.PriorFinding{}, nil, nil, false},
		{"unknown id", 2, []review.PriorFinding{{"other", "fixed", "Verified"}}, nil, nil, false},
		{"warning is not prior blocker", 2, []review.PriorFinding{{"F1", "fixed", "Verified"}, {"W1", "fixed", "Verified"}}, nil, nil, false},
		{"fixed", 2, []review.PriorFinding{{"F1", "fixed", "Verified"}}, nil, nil, true},
		{"no longer applies", 2, []review.PriorFinding{{"F1", "no_longer_applies", "Verified"}}, nil, nil, true},
		{"not fixed carried forward", 2, []review.PriorFinding{{"F1", "not_fixed", "Still broken"}}, []review.Finding{blocker}, nil, true},
		{"not fixed omitted", 2, []review.PriorFinding{{"F1", "not_fixed", "Still broken"}}, nil, nil, false},
		{"not fixed downgraded", 2, []review.PriorFinding{{"F1", "not_fixed", "Still broken"}}, []review.Finding{{ID: "F1", Severity: "warning", Title: "Bug", Details: "Broken"}}, nil, false},
		{"not fixed renamed", 2, []review.PriorFinding{{"F1", "not_fixed", "Still broken"}}, []review.Finding{{ID: "F2", Severity: "blocking", Title: "Bug", Details: "Broken"}}, nil, false},
		{"fixed remains blocking", 2, []review.PriorFinding{{"F1", "fixed", "Verified"}}, []review.Finding{blocker}, nil, false},
		{"dispute accepted", 2, []review.PriorFinding{{"F1", "no_longer_applies", "Verified"}}, nil, disputed, true},
		{"dispute rejected", 2, []review.PriorFinding{{"F1", "not_fixed", "Still broken"}}, []review.Finding{blocker}, disputed, true},
		{"dispute called fixed", 2, []review.PriorFinding{{"F1", "fixed", "Verified"}}, nil, disputed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			findings := tc.findings
			if findings == nil {
				findings = []review.Finding{}
			}
			status := "approved"
			for _, f := range findings {
				if f.Severity == "blocking" {
					status = "changes_required"
				}
			}
			report := review.Report{SchemaVersion: 2, Status: status, Findings: findings, PriorFindings: tc.prior}
			previous := []review.Finding{blocker, warning}
			if tc.round == 1 && !strings.HasPrefix(tc.name, "same round") {
				previous = nil
			}
			err := report.ValidatePriorFindings(tc.round, previous, tc.fix)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}
