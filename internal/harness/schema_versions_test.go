package harness_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/rcpassos/mergeyard/internal/contract"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/review"
)

// Each schema's required fields are the current table row for its phase, so a
// version bump cannot move one without the other.
func TestSchemasMatchCurrentRequirements(t *testing.T) {
	for _, tc := range []struct {
		phase  contract.Phase
		schema string
	}{
		{contract.Implement, string(harness.ImplementSchema())},
		{contract.Review, review.Schema},
		{contract.Fix, review.FixSchema},
	} {
		t.Run(string(tc.phase), func(t *testing.T) {
			var schema struct {
				Required   []string                   `json:"required"`
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if err := json.Unmarshal([]byte(tc.schema), &schema); err != nil {
				t.Fatal(err)
			}
			requirement, ok := contract.For(tc.phase, contract.CurrentVersion)
			if !ok || !slices.Equal(schema.Required, requirement.Fields) {
				t.Fatalf("required = %v, want %v", schema.Required, requirement.Fields)
			}
			if len(schema.Properties) != len(requirement.Fields) {
				t.Fatalf("schema has %d properties, contract requires %d", len(schema.Properties), len(requirement.Fields))
			}
			for _, field := range requirement.Fields {
				if schema.Properties[field] == nil {
					t.Fatalf("schema lacks required property %s", field)
				}
			}
		})
	}
}
