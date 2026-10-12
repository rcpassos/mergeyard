package acceptance_test

import (
	"reflect"
	"testing"

	"github.com/rcpassos/mergeyard/internal/acceptance"
)

func TestExtractCriteria(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []acceptance.Criterion
	}{
		{"ordered and checklist", "## ACCEPTANCE CRITERIA\r\n1. First\r\n2) [X] Second\r\n+ Third\r\n## Scope\r\n- Ignore", []acceptance.Criterion{{"AC1", "First"}, {"AC2", "Second"}, {"AC3", "Third"}}},
		{"no inference", "### Acceptance criteria\n- Ignore\n## Acceptance criteria\nFree prose", []acceptance.Criterion{}},
		{"fenced heading", "```md\n## Acceptance criteria\n- Ignore\n```\n## Acceptance criteria\n- Real", []acceptance.Criterion{{"AC1", "Real"}}},
		{"multiline criterion", "## Acceptance criteria\n- Works with\n  multiple lines\n- [ ] Next", []acceptance.Criterion{{"AC1", "Works with\nmultiple lines"}, {"AC2", "Next"}}},
		{"fenced example", "## Acceptance criteria\n- Real\n````md\n```\n- Ignore\n````\n- Next", []acceptance.Criterion{{"AC1", "Real"}, {"AC2", "Next"}}},
		{"empty checkbox", "## Acceptance criteria\n- [ ]\n- [x] \n- Real", []acceptance.Criterion{{"AC1", "Real"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := acceptance.Extract(tc.body)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v want %#v", got, tc.want)
			}
		})
	}
}

func TestReportIDsMatchIssueContract(t *testing.T) {
	expected := []acceptance.Criterion{{"AC1", "First"}, {"AC2", "Second"}}
	for _, tc := range []struct {
		name, data string
		valid      bool
	}{
		{"all", `[{"id":"AC2"},{"id":"AC1"}]`, true},
		{"omitted", `[{"id":"AC1"}]`, false},
		{"unknown", `[{"id":"AC1"},{"id":"AC3"}]`, false},
		{"duplicate", `[{"id":"AC1"},{"id":"AC1"}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := acceptance.ValidateIDs([]byte(tc.data), expected)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}
