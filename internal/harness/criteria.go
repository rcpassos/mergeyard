package harness

import (
	"context"
	"encoding/json"
	"path/filepath"

	"github.com/rcpassos/mergeyard/internal/acceptance"
)

// Save the contract separately from untrusted issue prose for restart validation.
func writeCriteria(ctx context.Context, writer FileWriter, phase PhaseContext, body string) error {
	data, err := json.Marshal(acceptance.Extract(body))
	if err != nil {
		return err
	}
	return writer.WriteFile(ctx, filepath.Join(phase.PhaseDir, "criteria.json"), data, 0600)
}
