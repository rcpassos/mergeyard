package review_test

import (
	"strconv"
	"testing"

	"github.com/rcpassos/mergeyard/internal/contract"
	"github.com/rcpassos/mergeyard/internal/review"
)

// Every persisted review version parses exactly its own contract, and the
// current version rejects what earlier versions allowed.
func TestReviewVersions(t *testing.T) {
	payloads := map[int]string{
		1: `{"schema_version":1,"status":"approved","summary":"Clean","findings":[]}`,
		2: `{"schema_version":2,"status":"approved","summary":"Clean","findings":[],"prior_findings":[]}`,
		3: `{"schema_version":3,"human_action":null,"status":"approved","summary":"Clean","findings":[],"prior_findings":[]}`,
		4: `{"schema_version":4,"criteria":[],"human_action":null,"status":"approved","summary":"Clean","findings":[],"prior_findings":[]}`,
	}
	if len(payloads) != contract.CurrentVersion {
		t.Fatalf("fixture covers %d versions, CurrentVersion is %d", len(payloads), contract.CurrentVersion)
	}
	for version, raw := range payloads {
		t.Run("v"+strconv.Itoa(version), func(t *testing.T) {
			report, err := review.ParseVersion([]byte(raw), version)
			if err != nil || report.SchemaVersion != version {
				t.Fatalf("saved contract rejected: %+v %v", report, err)
			}
			if report.PriorFindings == nil {
				t.Fatal("normalized prior_findings must be an empty array")
			}
			if _, err := review.ParseVersion([]byte(raw), version+1); err == nil {
				t.Fatal("later version accepted an older contract")
			}
			if version > 1 {
				if _, err := review.ParseVersion([]byte(payloads[version-1]), version); err == nil {
					t.Fatal("earlier contract accepted")
				}
			}
		})
	}
	if _, err := review.Parse([]byte(payloads[contract.CurrentVersion])); err != nil {
		t.Fatalf("current version rejected by Parse: %v", err)
	}
	if _, err := review.Parse([]byte(payloads[1])); err == nil {
		t.Fatal("new attempt accepted v1")
	}
	if report, err := review.ParseLegacy([]byte(payloads[1])); err != nil || len(report.PriorFindings) != 0 {
		t.Fatalf("legacy=%+v err=%v", report, err)
	}
}

// Every persisted fix version parses exactly its own contract.
func TestFixVersions(t *testing.T) {
	payloads := map[int]string{
		1: `{"schema_version":1,"status":"success","summary":"Done","responses":[]}`,
		2: `{"schema_version":2,"human_action":null,"status":"success","summary":"Done","responses":[]}`,
		3: `{"schema_version":3,"criteria":[],"human_action":null,"status":"success","summary":"Done","responses":[]}`,
		4: `{"schema_version":4,"verification":[],"criteria":[],"human_action":null,"status":"success","summary":"Done","responses":[]}`,
	}
	if len(payloads) != contract.CurrentVersion {
		t.Fatalf("fixture covers %d versions, CurrentVersion is %d", len(payloads), contract.CurrentVersion)
	}
	for version, raw := range payloads {
		t.Run("v"+strconv.Itoa(version), func(t *testing.T) {
			report, err := review.ParseFixVersion([]byte(raw), int64(version))
			if err != nil || report.SchemaVersion != version {
				t.Fatalf("saved contract rejected: %+v %v", report, err)
			}
			if _, err := review.ParseFixVersion([]byte(raw), int64(version+1)); err == nil {
				t.Fatal("later version accepted an older contract")
			}
			if version > 1 {
				if _, err := review.ParseFixVersion([]byte(payloads[version-1]), int64(version)); err == nil {
					t.Fatal("earlier contract accepted")
				}
			}
		})
	}
	if _, err := review.ParseFix([]byte(payloads[contract.CurrentVersion])); err != nil {
		t.Fatalf("current version rejected by ParseFix: %v", err)
	}
	if _, err := review.ParseFix([]byte(payloads[1])); err == nil {
		t.Fatal("new attempt accepted v1")
	}
}
