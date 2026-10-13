package review_test

import (
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/review"
)

func TestCriteriaVerdictsRequireBlockingEvidence(t *testing.T) {
	base := `{"schema_version":4,"human_action":null,"status":"approved","summary":"Checked","prior_findings":[],"findings":[],"criteria":[{"id":"AC1","verdict":"met","note":"go test ./... passed"}]}`
	for _, tc := range []struct {
		name, verdict, findings, status string
		valid                           bool
	}{
		{"met", "met", "[]", "approved", true},
		{"unverified", "unverified", "[]", "approved", true},
		{"not met without blocker", "not_met", "[]", "approved", false},
		{"not met with unrelated blocker", "not_met", `[{"id":"F1","severity":"blocking","title":"AC10 fails","details":"Unrelated","file":null,"line":null}]`, "changes_required", false},
		{"not met with warning", "not_met", `[{"id":"F1","severity":"warning","title":"AC1 fails","details":"Observed failure","file":null,"line":null}]`, "approved", false},
		{"not met backed", "not_met", `[{"id":"F1","severity":"blocking","title":"Criterion failed","details":"AC1: test failed","file":null,"line":null}]`, "changes_required", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := strings.Replace(base, `"verdict":"met"`, `"verdict":"`+tc.verdict+`"`, 1)
			raw = strings.Replace(raw, `"findings":[]`, `"findings":`+tc.findings, 1)
			raw = strings.Replace(raw, `"status":"approved"`, `"status":"`+tc.status+`"`, 1)
			_, err := review.Parse([]byte(raw))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestCriteriaStrictFields(t *testing.T) {
	for _, tc := range []struct {
		name, evidence, verdict string
		valid                   bool
	}{
		{"empty", `[]`, `[]`, true},
		{"valid", `[{"id":"AC1","met":true,"evidence":"go test passed"}]`, `[{"id":"AC1","verdict":"met","note":"go test passed"}]`, true},
		{"null array", `null`, `null`, false},
		{"object", `{}`, `{}`, false},
		{"null field", `[{"id":"AC1","met":null,"evidence":"test passed"}]`, `[{"id":"AC1","verdict":"met","note":null}]`, false},
		{"missing field", `[{"id":"AC1","met":true}]`, `[{"id":"AC1","verdict":"met"}]`, false},
		{"wrong type", `[{"id":"AC1","met":"true","evidence":"test passed"}]`, `[{"id":"AC1","verdict":true,"note":"test passed"}]`, false},
		{"blank detail", `[{"id":"AC1","met":true,"evidence":" "}]`, `[{"id":"AC1","verdict":"met","note":" "}]`, false},
		{"invalid identity", `[{"id":"other","met":true,"evidence":"test passed"}]`, `[{"id":"other","verdict":"met","note":"test passed"}]`, false},
		{"extra field", `[{"id":"AC1","met":true,"evidence":"test passed","extra":true}]`, `[{"id":"AC1","verdict":"met","note":"test passed","extra":true}]`, false},
		{"duplicate", `[{"id":"AC1","met":true,"evidence":"test passed"},{"id":"AC1","met":false,"evidence":"test failed"}]`, `[{"id":"AC1","verdict":"met","note":"test passed"},{"id":"AC1","verdict":"unverified","note":"not checked"}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fix := `{"schema_version":4,"verification":[],"human_action":null,"status":"success","summary":"Checked","responses":[],"criteria":` + tc.evidence + `}`
			_, err := review.ParseFix([]byte(fix))
			if (err == nil) != tc.valid {
				t.Fatalf("evidence valid=%v err=%v", tc.valid, err)
			}
			report := `{"schema_version":4,"human_action":null,"status":"approved","summary":"Checked","findings":[],"prior_findings":[],"criteria":` + tc.verdict + `}`
			_, err = review.Parse([]byte(report))
			if (err == nil) != tc.valid {
				t.Fatalf("verdict valid=%v err=%v", tc.valid, err)
			}
		})
	}
}
